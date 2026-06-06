/*
 * JuiceFS, Copyright 2020 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package meta

import "github.com/juicedata/juicefs/pkg/utils"

type slice struct {
	id    uint64
	size  uint32
	off   uint32
	len   uint32
	pos   uint32
	left  *slice
	right *slice
}

func newSlice(pos uint32, id uint64, cleng, off, len uint32) *slice {
	if len == 0 {
		return nil
	}
	s := &slice{}
	s.pos = pos
	s.id = id
	s.size = cleng
	s.off = off
	s.len = len
	s.left = nil
	s.right = nil
	return s
}

func (s *slice) read(buf []byte) {
	rb := utils.ReadBuffer(buf)
	s.pos = rb.Get32()
	s.id = rb.Get64()
	s.size = rb.Get32()
	s.off = rb.Get32()
	s.len = rb.Get32()
}

func (s *slice) cut(pos uint32) (left, right *slice) {
	if s == nil {
		return nil, nil
	}
	if pos <= s.pos {
		if s.left == nil {
			s.left = newSlice(pos, 0, 0, 0, s.pos-pos)
		}
		left, s.left = s.left.cut(pos)
		return left, s
	} else if pos < s.pos+s.len {
		l := pos - s.pos
		right = newSlice(pos, s.id, s.size, s.off+l, s.len-l)
		right.right = s.right
		s.len = l
		s.right = nil
		return s, right
	} else {
		if s.right == nil {
			s.right = newSlice(s.pos+s.len, 0, 0, 0, pos-s.pos-s.len)
		}
		s.right, right = s.right.cut(pos)
		return s, right
	}
}

func (s *slice) visit(f func(*slice)) {
	if s == nil {
		return
	}
	s.left.visit(f)
	right := s.right
	f(s) // s could be freed
	right.visit(f)
}

const sliceBytes = 24

func marshalSlice(pos uint32, id uint64, size, off, len uint32) []byte {
	w := utils.NewBuffer(sliceBytes)
	w.Put32(pos)
	w.Put64(id)
	w.Put32(size)
	w.Put32(off)
	w.Put32(len)
	return w.Bytes()
}

func readSlices(vals []string) []*slice {
	slices := make([]slice, len(vals))
	ss := make([]*slice, len(vals))
	for i, val := range vals {
		if len(val) != sliceBytes {
			logger.Errorf("corrupt slice: len=%d, val=%v", len(val), []byte(val))
			return nil
		}
		s := &slices[i]
		s.read([]byte(val))
		ss[i] = s
	}
	return ss
}

func readSliceBuf(buf []byte) []*slice {
	if len(buf)%sliceBytes != 0 {
		logger.Errorf("corrupt slices: len=%d", len(buf))
		return nil
	}
	nSlices := len(buf) / sliceBytes
	slices := make([]slice, nSlices)
	ss := make([]*slice, nSlices)
	for i := 0; i < len(buf); i += sliceBytes {
		s := &slices[i/sliceBytes]
		s.read(buf[i:])
		ss[i/sliceBytes] = s
	}
	return ss
}

// 将slice列表转换为逻辑上连续的视图
func buildSlice(ss []*slice) []Slice {
	var root *slice
	// 遍历所有的slice，后面覆盖前面
	for i := range ss {
		s := new(slice)
		*s = *ss[i]
		var right *slice
		// 将当前根节点在s.pos位置切开，得到左子树和右子树（包括s覆盖的内容和s之后的内容
		s.left, right = root.cut(s.pos)
		// 将右子树从新slice的终点处切分，得到的右子树是没有覆盖内容的部分
		_, s.right = right.cut(s.pos + s.len)
		// s 成为新的 root
		// 现在的结构是: [旧的左边内容] <--- s ---> [旧的右边内容]
		root = s
	}
	var pos uint32
	var chunk []Slice
	// 最后通过 root.visit 进行中序遍历。由于树的结构保证了 left < self < right 的位置关系，遍历结果自然就是按文件偏移量排序的、无重叠的最终文件片段
	root.visit(func(s *slice) {
		if s.pos > pos {
			chunk = append(chunk, Slice{Size: s.pos - pos, Len: s.pos - pos})
			pos = s.pos
		}
		chunk = append(chunk, Slice{Id: s.id, Size: s.size, Off: s.off, Len: s.len})
		pos += s.len
	})
	return chunk
}

func compactChunk(ss []*slice) (uint32, uint32, []Slice) {
	var chunk = buildSlice(ss)
	var pos uint32
	n := len(chunk)
	for n > 1 {
		if chunk[0].Id == 0 {
			pos += chunk[0].Len
			chunk = chunk[1:]
			n--
		} else if chunk[n-1].Id == 0 {
			chunk = chunk[:n-1]
			n--
		} else {
			break
		}
	}
	if n == 1 && chunk[0].Id == 0 {
		chunk[0].Len = 1
	}
	var size uint32
	for _, c := range chunk {
		size += c.Len
	}
	return pos, size, chunk
}

// 跳过一些不需要整理的slice
func skipSome(chunk []*slice) int {
	var skipped int
	var total = len(chunk)
OUT:
	for skipped < total {
		// 取出当前尚未被跳过的剩余 slice 列表
		ss := chunk[skipped:]
		// 模拟合并剩余的 slice
		// pos 合并后起始偏移量
		// size 合并后总有效数据大小
		// c 合并后的slice数组
		pos, size, c := compactChunk(ss)
		first := ss[0]
		// 如果第一个slice大小小于1MB
		// 或者第一个slice大小5倍小于合并后总有效数据大小
		// 或者合并后总有效数据大小为0
		// 不能跳过合并
		if first.len < (1<<20) || first.len*5 < size || size == 0 {
			// it's too small
			break
		}
		isFirst := func(pos uint32, s Slice) bool {
			return pos == first.pos && s.Id == first.id && s.Off == first.off && s.Len == first.len
		}
		// 对比合并后的第一个slice 与 合并前的第一个slice
		// 如果不一致，说明后面的 Slice 覆盖写了这个 first 的头部或改变了它的有效范围
		// 既然它被“污染”或截断了，那它就不能跳过，必须参与实际的合并重写
		if !isFirst(pos, c[0]) {
			// it's not the first slice, compact it
			break
		}
		// 遍历剩余 Slice
		// 如果发现后面有一个 Slice 的各项属性和当前的 first 一模一样，这通常意味着冗余写入或者某种特殊的重试/异常状态
		// 直接退出，进行实际的合并重写
		for _, s := range ss[1:] {
			if *s == *first {
				break OUT
			}
		}
		skipped++
	}
	return skipped
}
