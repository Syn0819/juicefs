# JuiceFS Rename 实现原理与代码分析

本文详细分析 JuiceFS 中 `rename`（重命名/移动）的完整实现原理与代码路径，涵盖 FUSE 入口、VFS 层、路径解析、元数据接口以及 Redis/SQL/TKV 三种元数据引擎的 `doRename` 实现。

---

## 一、整体架构与调用链

Rename 的请求自内核经 FUSE 进入 JuiceFS，沿以下路径执行：

```
用户态: mv /a/x /b/y
    ↓
内核 VFS → FUSE 内核模块
    ↓
pkg/fuse/fuse.go  fileSystem.Rename()
    ↓
pkg/vfs/vfs.go    VFS.Rename()
    ↓
pkg/meta (base/redis/sql/tkv)  Meta.Rename() → doRename()
    ↓
元数据引擎 (Redis/SQL/TiKV/...) 事务内更新目录项与 inode
```

**要点**：JuiceFS 的 rename 是**纯元数据操作**，只改目录项（parent + name → inode 的映射）和少量 inode 属性（如 Parent、Ctime、Nlink），**不迁移或拷贝对象存储上的数据**。文件数据由 inode 引用，inode 不变则数据不变。

---

## 二、Rename 语义与 Flags

### 2.1 接口定义（meta/interface.go）

```go
Rename(ctx, parentSrc, nameSrc, parentDst, nameDst, flags, inode, attr) syscall.Errno
```

- **parentSrc, nameSrc**：源目录 inode + 源条目名。
- **parentDst, nameDst**：目标目录 inode + 目标条目名。
- **flags**：控制行为，见下。
- **inode, attr**：出参，返回被移动条目的 inode 和属性（可选）。

### 2.2 Flags 常量（meta/interface.go）

```go
const (
	RenameNoReplace = 1 << iota  // 目标已存在则报 EEXIST，不覆盖
	RenameExchange               // 交换两目录项（Linux renameat2 RENAME_EXCHANGE）
	RenameWhiteout               // 暂不支持，返回 ENOTSUP
	RenameRestore                // 内部用：从回收站恢复
)
```

- **flags == 0**：常规 rename，若目标存在则覆盖（文件或空目录）。
- **RenameNoReplace**：目标已存在则返回 `EEXIST`（常用于 Hadoop 等不覆盖语义）。
- **RenameExchange**：交换源与目标两个目录项。
- **RenameRestore**：与回收站配合，允许目标在 trash 下等特殊校验。

base 层会校验 flags 合法组合（如 `RenameNoReplace | RenameRestore`），非法组合返回 `EINVAL`。

---

## 三、各层代码实现

### 3.1 FUSE 层：pkg/fuse/fuse.go

```go
func (fs *fileSystem) Rename(cancel <-chan struct{}, in *fuse.RenameIn, oldName string, newName string) (code fuse.Status) {
	ctx := fs.newContext(cancel, &in.InHeader)
	defer releaseContext(ctx)
	err := fs.v.Rename(ctx, Ino(in.NodeId), oldName, Ino(in.Newdir), newName, in.Flags)
	return fuse.Status(err)
}
```

- **in.NodeId**：源父目录的 inode（即 `parentSrc`）。
- **in.Newdir**：目标父目录的 inode（即 `parentDst`）。
- **oldName / newName**：源、目标文件名。
- **in.Flags**：对应 Linux `renameat2` 的 flags（如 `RENAME_NOREPLACE`、`RENAME_EXCHANGE`）。

FUSE 层只做上下文创建和参数透传，无额外逻辑。

---

### 3.2 VFS 层：pkg/vfs/vfs.go

```go
func (v *VFS) Rename(ctx Context, parent Ino, name string, newparent Ino, newname string, flags uint32) (err syscall.Errno) {
	// 1. 根目录保留名、名字长度校验
	if parent == rootID && IsSpecialName(name) { return syscall.EPERM }
	if newparent == rootID && IsSpecialName(newname) { return syscall.EPERM }
	if len(name) > maxName || len(newname) > maxName { return syscall.ENAMETOOLONG }

	var inode Ino
	var attr = &Attr{}
	err = v.Meta.Rename(ctx, parent, name, newparent, newname, flags, &inode, attr)
	if err == 0 {
		// 2. 使目录句柄缓存与元数据一致
		v.invalidateDirHandle(parent, name, 0, nil)   // 源目录：删除 name 条目
		v.invalidateDirHandle(newparent, newname, 0, nil) // 目标目录：删除旧 newname（若有）
		v.invalidateDirHandle(newparent, newname, inode, attr) // 目标目录：插入新条目
	}
	return
}
```

**VFS 职责**：

1. **保留名与长度**：禁止对根目录下特殊名（如 `.trash`）rename，并限制名字长度。
2. **调用 Meta.Rename**：真正逻辑在 meta 层。
3. **目录缓存一致性**：通过 `invalidateDirHandle` 更新所有已打开目录句柄的 readdir 缓存，保证后续 readdir 不丢、不重。

**invalidateDirHandle**（pkg/vfs/handle.go）：对指定 parent 下所有打开的目录 handle，若 `inode > 0` 则在该 handle 的 DirHandler 中 **Insert(inode, name, attr)**，否则 **Delete(name)**。这样正在 readdir 的客户端能立刻看到 rename 结果。

---

### 3.3 路径版 API：pkg/fs/fs.go（juicefs mount 以外使用）

`FileSystem.Rename(ctx, oldpath, newpath, flags)` 用于按路径调用的场景（如 S3 网关、WebDAV、SDK）：

```go
func (fs *FileSystem) Rename(ctx meta.Context, oldpath string, newpath string, flags uint32) (err syscall.Errno) {
	oss := trimDotsForRename(strings.Split(oldpath, "/"))
	nss := trimDotsForRename(strings.Split(newpath, "/"))
	// 防止 oldpath 是 newpath 的祖先（如 rename /a 到 /a/b）
	for i := 0; i < len(oss); {
		if i >= len(nss) || oss[i] != nss[i] { break }
		i++
		if i == len(oss) && i < len(nss) { err0 = syscall.EINVAL; break } // /a -> /a/xxx 非法
	}
	oldfi, _ := fs.resolve(ctx, parentDir(oldpath), true)  // 解析源父目录
	newfi, _ := fs.resolve(ctx, parentDir(newpath), true) // 解析目标父目录
	err = fs.m.Rename(ctx, oldfi.inode, path.Base(oldpath), newfi.inode, path.Base(newpath), flags, nil, nil)
	fs.InvalidateEntry(oldfi.inode, path.Base(oldpath))
	fs.InvalidateEntry(newfi.inode, path.Base(newpath))
	return
}
```

- **trimDotsForRename**：规范化路径中的 `.` 和 `..`。
- **resolve**：得到父目录 inode；再取 `path.Base` 得到 name，转为 inode 版 Rename。
- **InvalidateEntry**：使该父目录的目录缓存失效，与 VFS 的 invalidateDirHandle 目的一致。

---

## 四、Meta 层：baseMeta.Rename（pkg/meta/base.go）

所有引擎的 `Rename` 都经 `baseMeta.Rename` 统一前置/后置处理，再调用具体引擎的 `doRename`。

### 4.1 前置校验与配额

```go
func (m *baseMeta) Rename(...) syscall.Errno {
	// 禁止对 .trash 或从/到回收站的非法 rename
	if parentSrc == RootInode && nameSrc == TrashName || parentDst == RootInode && nameDst == TrashName {
		return syscall.EPERM
	}
	if parentDst.IsTrash() || (parentSrc.IsTrash() && ctx.Uid() != 0) {
		return syscall.EPERM
	}
	if m.conf.ReadOnly { return syscall.EROFS }
	if errno := checkInodeName(nameDst); errno != 0 { return errno }

	// flags 合法性
	switch flags {
	case 0, RenameNoReplace, RenameExchange, RenameNoReplace | RenameRestore:
	case RenameWhiteout, RenameNoReplace | RenameWhiteout:
		return syscall.ENOTSUP
	default:
		return syscall.EINVAL
	}

	parentSrc = m.checkRoot(parentSrc)
	parentDst = m.checkRoot(parentDst)

	// 配额：若源、目标所在配额树不同，先解析源 inode 并计算 space/inodes
	var quotaSrc, quotaDst Ino
	quotaSrc, _ = m.getQuotaParent(ctx, parentSrc)
	quotaDst = ...
	if quotaSrc != quotaDst {
		m.Lookup(ctx, parentSrc, nameSrc, inode, attr, false)
		// 目录用 GetSummary 算空间，文件用 align4K(attr.Length)
		if quotaDst > 0 && m.checkDirQuota(ctx, parentDst, space, inodes) {
			return syscall.EDQUOT
		}
	}

	st := m.en.doRename(ctx, parentSrc, nameSrc, parentDst, nameDst, flags, inode, tinode, attr, tattr)
	// ...
}
```

- **checkRoot**：处理“子目录挂载视图”到真实 inode 的映射。
- **配额**：仅在源、目标属于不同配额子树时检查目标配额是否足够；同一子树内移动不改变配额占用。

### 4.2 doRename 成功后的收尾

```go
	if st == 0 {
		// 目录的父缓存
		if attr.Typ == TypeDirectory {
			m.dirParents[*inode] = parentDst
		}
		// 跨目录移动：更新两个目录的 dirStat（逻辑长度、空间、条目数）
		if parentSrc != parentDst {
			m.updateDirStat(ctx, parentSrc, -diffLength, -align4K(diffLength), -1)
			m.updateDirStat(ctx, parentDst, +diffLength, +align4K(diffLength), 1)
			// 配额树变更时更新配额占用
			if quotaSrc != quotaDst { ... }
		}
		// 若覆盖了目标条目（*tinode > 0）且非 Exchange：从目标目录和配额中扣减被覆盖项
		if *tinode > 0 && flags != RenameExchange {
			m.updateDirStat(ctx, parentDst, ...)
			m.updateDirQuota(ctx, parentDst, ...)
			m.updateUserGroupQuota(...)
		}
	}
	return st
```

- **dirParents**：目录 inode → 父目录 inode 的缓存，rename 目录后更新。
- **updateDirStat**：维护目录的逻辑长度、4K 对齐空间、子项数量，供统计与配额用。
- 被覆盖的目标条目若是文件/目录，其空间与 inode 从目标目录和配额中扣减。

---

## 五、doRename 核心逻辑（以 TKV 为例）

下面以 **pkg/meta/tkv.go** 的 `kvMeta.doRename` 为主线说明事务内的步骤；Redis/SQL 的 `doRename` 与之一致，仅存储 API 不同（Redis: HGet/HSet/HDel/ZAdd/Del；SQL: SELECT/UPDATE/DELETE/INSERT）。

### 5.1 事务与锁

- 使用 **m.txn(ctx, func(tx *kvTxn) { ... }, parentLocks...)**，对 `parentDst` 以及（若不在 trash）`parentSrc` 加锁，保证并发 rename 的互斥与可串行化。
- Redis 用 WATCH + 事务；SQL 用数据库事务；TKV 用事务 API。

### 5.2 查找源条目与快速路径

```go
buf := tx.get(m.entryKey(parentSrc, nameSrc))
if buf == nil && m.conf.CaseInsensi {
	// 大小写不敏感：resolveCase 解析真实 nameSrc
}
if buf == nil { return syscall.ENOENT }
typ, ino := m.parseEntry(buf)  // 源类型与 inode

if parentSrc == parentDst && nameSrc == nameDst {
	*inode = ino
	return nil  // 同目录同名，无操作
}
```

- **entryKey(parent, name)**：该父目录下 name 对应的存储 key（不同引擎实现不同：Redis 为 Hash 的 field，TKV 为独立 key）。
- **parseEntry(buf)**：得到 Type（文件/目录/符号链接等）和 Ino。

### 5.3 加载并校验三个 inode 属性

```go
rs := tx.gets(m.inodeKey(parentSrc), m.inodeKey(parentDst), m.inodeKey(ino))
// sattr = 源父目录, dattr = 目标父目录, iattr = 源条目 inode
```

- **源父、目标父必须是目录**（TypeDirectory），否则 `ENOTDIR`。
- **Access(ctx, parentSrc, MODE_MASK_W|MODE_MASK_X)**：当前用户对源父目录有写+执行权限。
- **Access(ctx, parentDst, ...)**：对目标父目录有写+执行权限。
- 若目标父在回收站外且 `dattr.Parent > TrashInode`，且非 Restore，返回 `ENOENT`。
- **ino == parentDst** 或 **ino == dattr.Parent**：禁止把目录 rename 成自己的子目录或造成循环，返回 `EPERM`。
- **FlagAppend/FlagImmutable**：源父、目标父、源 inode 若带这些标志，返回 `EPERM`。
- **Sticky 位**：若源父目录有 sticky，且非 root 且非源文件属主等，则不能删除/覆盖该条目，返回 `EACCES`。

### 5.4 目标条目存在时的处理

```go
dbuf := tx.get(m.entryKey(parentDst, nameDst))
if dbuf != nil {
	if flags&RenameNoReplace != 0 { return syscall.EEXIST }
	dtyp, dino := m.parseEntry(dbuf)
	// 加载目标 inode 属性 tattr
	// 若目标有 Append/Immutable 或 SkipTrash 等，做相应处理
```

- **RenameNoReplace**：目标存在则直接返回 EEXIST。
- **RenameExchange**：
  - 交换两目录项：把目标条目“移”到源位置，源条目“移”到目标位置。
  - 若跨目录，需更新目标条目的 Parent、源父与目标父的 Nlink；更新 parentKey 计数等。
- **普通覆盖**：
  - 若目标是**非空目录**：返回 `ENOTEMPTY`。
  - 若源是目录、目标是文件（或反过来）：返回 `ENOTDIR` / `EISDIR`。
  - 若目标是**空目录**：目标目录 inode 的 Nlink--，目标父 Nlink--；若开启回收站则把目标移到 trash，否则后面会删除该 inode 及相关 key。
  - 若目标是**文件**：Nlink--；若 Nlink 变为 0 且无打开句柄，则加入延迟删除（delfile）或直接删 inode；若仍被打开则 sustained。
  - 若目标是**符号链接**：删 symKey(dino)，再删 inode。

### 5.5 更新源条目的父与时间

```go
if parentSrc != parentDst {
	if typ == TypeDirectory {
		iattr.Parent = parentDst
		sattr.Nlink--
		dattr.Nlink++
	}
	else if iattr.Parent > 0 {
		iattr.Parent = parentDst
	}
}
iattr.Ctime = now
// 若源父/目标父需要更新 mtime（目录内容变化），supdate/dupdate = true
```

- 目录的 **Parent** 必须正确，以便 getParent、配额、trash 等逻辑使用。
- **Nlink**：目录的父目录会记录子目录数，跨目录移动时两边目录的 Nlink 要增减。

### 5.6 写回元数据（TKV 示例）

```go
if exchange {
	tx.set(m.entryKey(parentSrc, nameSrc), dbuf)           // 源位置写目标条目
	tx.set(m.inodeKey(dino), m.marshal(&tattr))
	// parentKey(dino) 的 parentSrc += 1, parentDst -= 1
} else {
	tx.delete(m.entryKey(parentSrc, nameSrc))              // 删源目录项
	if dino > 0 {
		if trash > 0 {
			tx.set(m.entryKey(trash, m.trashEntry(...)), dbuf)
			tx.set(m.inodeKey(dino), ...)
			// parentKey(dino): trash += 1, parentDst -= 1
		} else if 目标为文件且 Nlink>0 {
			tx.set(m.inodeKey(dino), ...)
			// parentKey(dino): parentDst -= 1
		} else {
			// 删除目标 inode、xattr、parentKey、delfile 或 sustained 等
		}
		if dtyp == TypeDirectory {
			tx.delete(m.dirQuotaKey(dino))
		}
	}
}
if parentDst != parentSrc {
	if supdate { tx.set(m.inodeKey(parentSrc), m.marshal(&sattr)) }
	// parentKey(ino): parentDst += 1, parentSrc -= 1
}
tx.set(m.inodeKey(ino), m.marshal(&iattr))
tx.set(m.entryKey(parentDst, nameDst), buf)   // 目标目录下写入源条目
if dupdate { tx.set(m.inodeKey(parentDst), m.marshal(&dattr)) }
```

- **exchange**：源位置存原目标条目，目标位置存原源条目；两边 inode 的 Parent 与 parentKey 计数同步更新。
- **非 exchange**：删除源目录项；若目标条目存在则或移入 trash 或删 inode/扩展属性/目录配额等；最后在目标目录下写入源条目，并更新源 inode 的 Parent 与 parentKey。

### 5.7 回收站（trash）

- **checkTrash(parentDst, &trash)**：若目标父目录所在卷启用回收站且目标父在回收站内，则得到 trash inode；否则 `trash = 0`。
- 当 `trash > 0` 且要“覆盖”的目标是文件或空目录时，不立即删目标 inode，而是把目标条目移到 `trash` 目录下（`trashEntry(parentDst, dino, nameDst)`），并更新目标的 Parent 和 parentKey，便于后续 restore 或定时清理。

---

## 六、Redis 与 SQL 的 doRename 差异摘要

- **Redis**：`entryKey(parent)` 为 Hash key，条目名为 field；用 `HGet/HSet/HDel` 读写目录项；用 `WATCH` + 事务保证原子性；删除文件用 `ZAdd(delfiles(), ...)` 做延迟删除。
- **SQL**：目录项通常存在一张 `junction`/`dir_entry` 表，用 (parent_ino, name) 做主键或唯一索引；在事务里 `SELECT ... FOR UPDATE` 后 `UPDATE/INSERT/DELETE`。
- **TKV**：每个目录项一个 key（如 `entryKey(parentSrc, nameSrc)`），事务内 get/set/delete；结构上与上面描述一致。

三种引擎的**语义**一致：都是“在事务内改目录项 + inode 属性 + 可选回收站/删除”，不涉及数据面。

---

## 七、目录缓存一致性小结

| 层级 | 机制 |
|------|------|
| VFS | Rename 成功后对源父、目标父调用 `invalidateDirHandle`，更新所有已打开目录句柄的 DirHandler（Insert/Delete 条目），使 readdir 与元数据一致。 |
| fs (路径 API) | `InvalidateEntry(parent, name)` 使该父目录的目录缓存失效，下次 readdir 会重新从 meta 拉取。 |
| Meta | 无额外缓存；依赖引擎事务保证持久化一致性。 |

---

## 八、错误码与边界情况

| 场景 | 错误码 |
|------|--------|
| 源不存在 | ENOENT |
| 源父或目标父不是目录 | ENOTDIR |
| 目标存在且 flags 含 RenameNoReplace | EEXIST |
| 目标是非空目录（覆盖） | ENOTEMPTY |
| 源是目录、目标是文件（或反） | EISDIR / ENOTDIR |
| 权限（写目录、sticky、immutable 等） | EACCES / EPERM |
| 目标在回收站外但 dattr 在 trash 下 | ENOENT |
| 跨配额树且目标配额不足 | EDQUOT |
| 只读文件系统 | EROFS |
| RenameWhiteout | ENOTSUP |

---

## 九、总结

- **Rename 只改元数据**：目录项（parent+name → inode）和 inode 的 Parent、Ctime、Nlink 等；不拷贝或迁移对象存储数据。
- **调用链**：FUSE → VFS.Rename → Meta.Rename (base) → 各引擎 doRename；路径版 API 先 resolve 再调 Meta.Rename。
- **base 层**：做权限/配额/trash/只读/flags 校验，以及 rename 成功后的 dirStat、配额、dirParents 更新。
- **doRename**：在事务内完成“查源/目标 → 权限与语义校验 → 覆盖/交换/回收站 → 写回目录项与 inode”；支持 RenameNoReplace、RenameExchange、RenameRestore 与回收站。
- **一致性**：通过 invalidateDirHandle/InvalidateEntry 保证目录读缓存与元数据一致；通过引擎事务保证元数据持久化一致。

以上即为 JuiceFS 中 rename 的完整实现原理与代码分析。
