package meta

import (
	"sync"
	"time"
)

const (
	invalidateAllChunks = 0xFFFFFFFF
	invalidateAttrOnly  = 0xFFFFFFFE
)

// 用了 Go 语言标准库的 sync.Pool 进行对象池化
var ofPool = sync.Pool{
	New: func() interface{} {
		return &openFile{}
	},
}

type openFile struct {
	sync.RWMutex
	attr Attr
	refs int
	// 最后一次校验/更新元数据的时间戳，用于判断缓存是否过期
	lastCheck int64
	// 针对第一个chunk的优化，单独存储
	// 因为绝大部分文件都很小，只有一个chunk，可以节省map的开销
	first  []Slice
	chunks map[uint32][]Slice
}

func (o *openFile) invalidateChunk() {
	o.first = nil
	for c := range o.chunks {
		delete(o.chunks, c)
	}
}

// 放回内存池
func (o *openFile) release() {
	o.attr = Attr{}
	o.refs = 0
	o.lastCheck = 0
	o.first = nil
	o.chunks = nil
	ofPool.Put(o)
}

type openfiles struct {
	sync.Mutex
	expire time.Duration
	limit  uint64
	files  map[Ino]*openFile
}

func newOpenFiles(expire time.Duration, limit uint64) *openfiles {
	of := &openfiles{
		expire: expire,
		limit:  limit,
		files:  make(map[Ino]*openFile),
	}
	go of.cleanup()
	return of
}

// 在全局句柄缓存管理器初始化时启动
// 后台协程实现高效且相对平滑的缓存淘汰策略
func (o *openfiles) cleanup() {
	for {
		var (
			cnt, deleted, todel int
			candidateIno        Ino
			candidateOf         *openFile
		)
		o.Lock()
		// 检查缓存数量是否超过了 limit。如果超限，需要计算出需要删除的数量 todel
		if o.limit > 0 && len(o.files) > int(o.limit) {
			todel = len(o.files) - int(o.limit)
		}
		now := time.Now().Unix()
		// 通过 range o.files 遍历字典。为了防止单次锁占用时间过长阻塞正常读写，单次最多只遍历 1000 个文件
		for ino, of := range o.files {
			cnt++
			if cnt > 1e3 || todel > 0 && deleted >= todel {
				break
			}
			if of.refs <= 0 {
				// 如果 refs <= 0（文件已关闭）且超过 12 小时没有被访问过，直接释放并删除
				if now-of.lastCheck > 3600*12 {
					of.release()
					delete(o.files, ino)
					deleted++
					continue
				}
				if todel == 0 {
					continue
				}
				if candidateIno == 0 {
					candidateIno = ino
					candidateOf = of
					continue
				}
				// 如果当前缓存数超过了 limit，它会在遍历过程中比较相邻文件的 lastCheck 时间戳
				// 每次都将较旧的那个（即更久未被访问的）淘汰掉，直到腾出足够的空间
				if of.lastCheck < candidateOf.lastCheck {
					candidateIno = ino
					candidateOf = of
				}
				candidateOf.release()
				delete(o.files, candidateIno)
				deleted++
				candidateIno = 0
			}
		}
		o.Unlock()
		// time.Sleep 的时间是根据遍历和删除的比例动态计算的
		// 工作量大时休息时间短，工作量小时休息时间长，巧妙地平衡了 CPU 占用和清理效率
		time.Sleep(time.Millisecond * time.Duration(1000*(cnt+1-deleted*2)/(cnt+1)))
	}
}

// 尝试打开，如果缓存存在其没过期，则直接返回句柄
func (o *openfiles) OpenCheck(ino Ino, attr *Attr) bool {
	o.Lock()
	defer o.Unlock()
	of, ok := o.files[ino]
	if ok && time.Second*time.Duration(time.Now().Unix()-of.lastCheck) < o.expire {
		if attr != nil {
			*attr = of.attr
		}
		of.refs++
		return true
	}
	return false
}

func (o *openfiles) Open(ino Ino, attr *Attr) {
	o.Lock()
	defer o.Unlock()
	of, ok := o.files[ino]
	if !ok {
		// 缓存中没有，从内存池获取
		of = ofPool.Get().(*openFile)
		o.files[ino] = of
	} else if attr != nil && attr.Mtime == of.attr.Mtime && attr.Mtimensec == of.attr.Mtimensec {
		attr.KeepCache = of.attr.KeepCache
	} else {
		// 如果文件被修改过，需要使原来的句柄失效
		of.invalidateChunk()
	}
	if attr != nil {
		of.attr = *attr
	}
	// next open can keep cache if not modified
	of.attr.KeepCache = true
	of.refs++
	of.lastCheck = time.Now().Unix()
}

func (o *openfiles) Close(ino Ino) bool {
	o.Lock()
	defer o.Unlock()
	of, ok := o.files[ino]
	if ok {
		of.refs--
		return of.refs <= 0
	}
	return true
}

// 读取元数据。只有当 lastCheck 在 expire 期限内 元数据才算有效
func (o *openfiles) Check(ino Ino, attr *Attr) bool {
	if attr == nil {
		panic("attr is nil")
	}
	o.Lock()
	defer o.Unlock()
	of, ok := o.files[ino]
	if ok && time.Second*time.Duration(time.Now().Unix()-of.lastCheck) < o.expire {
		*attr = of.attr
		return true
	}
	return false
}

// 更新元数据。这里同样会比对修改时间（Mtime 和 Mtimensec）
// 如果发现文件被其他客户端修改了，会立即清空对应的 Chunk 缓存
func (o *openfiles) Update(ino Ino, attr *Attr) bool {
	if attr == nil {
		return false
	}
	o.Lock()
	defer o.Unlock()
	of, ok := o.files[ino]
	if ok {
		if attr.Mtime != of.attr.Mtime || attr.Mtimensec != of.attr.Mtimensec {
			of.invalidateChunk()
		} else {
			attr.KeepCache = of.attr.KeepCache
		}
		of.attr = *attr
		of.lastCheck = time.Now().Unix()
		return true
	}
	return false
}

func (o *openfiles) IsOpen(ino Ino) bool {
	o.Lock()
	defer o.Unlock()
	of, ok := o.files[ino]
	return ok && of.refs > 0
}

func (o *openfiles) ReadChunk(ino Ino, indx uint32) ([]Slice, bool) {
	o.Lock()
	defer o.Unlock()
	of, ok := o.files[ino]
	if !ok {
		return nil, false
	}
	if indx == 0 {
		return of.first, of.first != nil
	} else {
		cs, ok := of.chunks[indx]
		return cs, ok
	}
}

func (o *openfiles) CacheChunk(ino Ino, indx uint32, cs []Slice) {
	o.Lock()
	defer o.Unlock()
	of, ok := o.files[ino]
	if !ok {
		return
	}
	if indx == 0 {
		of.first = cs
	} else {
		if of.chunks == nil {
			of.chunks = make(map[uint32][]Slice)
		}
		of.chunks[indx] = cs
	}
}

func (o *openfiles) InvalidateChunk(ino Ino, indx uint32) {
	o.Lock()
	defer o.Unlock()
	of, ok := o.files[ino]
	if ok {
		if indx == invalidateAllChunks {
			of.invalidateChunk()
		} else if indx == 0 {
			of.first = nil
		} else {
			delete(of.chunks, indx)
		}
		of.lastCheck = 0
	}
}

func (o *openfiles) find(ino Ino) *openFile {
	o.Lock()
	defer o.Unlock()
	return o.files[ino]
}
