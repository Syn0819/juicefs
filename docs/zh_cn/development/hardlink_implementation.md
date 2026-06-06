# JuiceFS 硬链接（Hardlink）实现原理与代码分析

本文档详细分析 JuiceFS 中硬链接的完整实现，包括设计原理、数据结构、调用链以及各元数据后端的实现差异。

---

## 一、设计原理

### 1.1 硬链接语义

- **定义**：硬链接是同一 inode 在目录树中的多个命名入口，即多组 `(parent, name)` 指向同一个 inode。
- **共享**：所有硬链接共享同一份文件元数据（attr）和文件数据（chunk/slice），因此：
  - 修改任一链接看到的都是同一文件内容；
  - 删除某个链接只是减少该 inode 的引用计数，只有计数归零时才真正删除 inode 与数据。

### 1.2 核心数据结构

#### Attr 中的相关字段（`pkg/meta/interface.go`）

```go
// 链接数：目录 = 2 + 子目录数；普通文件 = 硬链接数
Nlink uint32

// 父目录 inode：0 表示由 parentKey 单独维护（多父/硬链接场景）
Parent Ino
```

- **Nlink**：当前 inode 的链接计数。创建硬链接时 +1，unlink 时 -1；为 0 时 inode 可被回收。
- **Parent**：
  - **单父**：普通文件只有一个父目录时，直接存 `attr.Parent = parent`。
  - **多父**：一旦创建了硬链接，同一 inode 会有多个父目录，无法用单一 Parent 表示，因此约定 **`Parent = 0`**，并由 **LinkParent / parentKey** 结构单独记录「该 inode 被哪些 parent 引用、每个 parent 下有几条链接」。

#### LinkParent（文档中的概念，对应实现中的 parentKey）

- **作用**：当 `attr.Parent == 0` 时，记录「inode → (parentInode, count)」。
- **含义**：`count` 表示该 parent 目录下指向该 inode 的目录项数量（同一目录下可有多个同名硬链接的计数，一般每条 link 对应 1）。
- **用途**：目录统计、配额、回收站、GetParents 等需要「知道这个 inode 出现在哪些目录下」的逻辑。

文档描述（`docs/zh_cn/development/internals.md`）：

```text
inode -> parentInode, links
```

同一目录下可有多条硬链接指向同一 inode，因此用 `links` 计数。

### 1.3 与命名空间的关系

- **Edge / Dentry**：`(parent, name) -> (type, inode)`。创建硬链接 = 在目标目录下新增一条 edge，**inode 不变**。
- **Node/Attr**：一个 inode 对应一份 attr；硬链接只增加 Nlink 并可能把 Parent 改为 0，同时维护 parentKey。

---

## 二、调用链概览

```
用户态/内核
    │
    ├─ FUSE: Link(父目录 inode, 新名字, 源 inode)
    │       pkg/fuse/fuse.go  fs.fileSystem.Link
    │           → fs.v.Link(ctx, in.Oldnodeid, in.NodeId, name)
    │
    ├─ VFS: pkg/vfs/vfs.go  VFS.Link
    │       → v.Meta.Link(ctx, ino, newparent, newname, attr)
    │
    ├─ Meta 统一入口: pkg/meta/base.go  baseMeta.Link
    │       → 权限/配额/类型检查 → m.en.doLink(...)
    │
    └─ 各引擎: doLink
            Redis: pkg/meta/redis.go  redisMeta.doLink
            TKV:   pkg/meta/tkv.go   kvMeta.doLink
            SQL:   pkg/meta/sql.go   dbMeta.doLink
```

另外，**Hadoop/对象存储 API** 通过 `pkg/fs/fs.go` 的 `FileSystem.Link(ctx, srcPath, dstPath)`：先 resolve 得到源 inode 和目标父目录 inode，再调用 `m.Link(ctx, fi.inode, pi.inode, path.Base(dst), nil)`。

---

## 三、baseMeta.Link：统一入口逻辑

**位置**：`pkg/meta/base.go` 约 1558–1604 行。

### 3.1 前置校验

- 禁止在 trash 下或对根目录的 `.trash` 做 Link。
- 只读模式、名字非法、`"."`/`".."` 等直接返回。
- **GetAttr(inode)** 取源 inode 的 attr；若源是**目录**则返回 `EPERM`（不允许对目录硬链接）。
- **配额**：用户配额、组配额、目标目录配额各检查「+1 inode、+0 空间」（硬链接不占新空间）。

### 3.2 调用引擎与事后统计

- 调用 `m.en.doLink(ctx, inode, parent, name, attr)` 完成真正的元数据修改。
- 成功后：
  - `updateDirStat(parent, +length, +align4K(length), +1)`；
  - `updateDirQuota(parent, +align4K(length), +1)`；
  - `updateUserGroupQuota(attr.Uid, attr.Gid, 0, 1)`（只 +1 inode，不增空间）。

硬链接不分配新数据块，因此「目录统计 / 配额」里只增加 inode 数和目录下「逻辑文件数」，空间增加为 0（或按原文件长度做目录统计，取决于现有 DirStat 设计）。

---

## 四、各后端的 doLink 实现

### 4.1 TKV 引擎（`pkg/meta/tkv.go`）

#### 4.1.1 parentKey 格式

```go
// parentKey(inode, parent) -> 存 count（该 parent 下指向 inode 的链接数）
func (m *kvMeta) parentKey(inode, parent Ino) []byte {
    return m.fmtKey("A", inode, "P", parent)
}
```

即 Key 为 `A{inode}P{parent}`，Value 为计数（如 int64 打包）。

**parentKey 只存了「该 parent 下指向 inode 的链接数」，没有存具体文件名（name）。** 要找「某个 inode 的所有硬链接的完整路径」时，需要同时用到 parentKey 和目录项（Edge）：

- **parentKey**：通过 `GetParents(ctx, inode)`（内部查 parentKey / LinkParent）得到「哪些 parent 目录包含该 inode」以及每个 parent 下的链接数。
- **名字从哪里来**：目录项（Edge）存的是 `(parent, name) -> (type, inode)`，即「父目录 + 文件名」指向 inode。所以对每个 parent，只要对该目录做一次 **Readdir**，在返回的条目里找 `e.Inode == inode` 的项，其 `e.Name` 就是该 parent 下指向该 inode 的文件名。再结合 parent 自己的路径，就能拼出完整路径。

**是否有「查所有硬链接路径」的场景**：有。Meta 接口提供 **`GetPaths(ctx, inode) []string`**（`pkg/meta/interface.go:523`），返回该 inode 的所有路径。实现逻辑（`pkg/meta/base.go:2233`）：先 `GetParents(inode)` 得到所有 parent 及 count，再对每个 parent 做 `doReaddir(parent)` 得到该目录下所有条目，筛出 `e.Inode == inode` 的条目得到 name，最后用 `path.Join(父目录路径, name)` 得到一条完整路径。该 API 在配额、统计、以及需要向用户展示「这个文件还有哪些路径」时会被使用（如 `pkg/meta/quota.go` 等）。

#### 4.1.2 doLink 步骤（约 2105–2165 行）

1. **事务内**：
   - `tx.gets(inodeKey(parent), inodeKey(inode))`：校验父目录和源 inode 存在且父为目录、非 trash 等。
   - Access 检查父目录写+执行权限；父目录不可 Immutable。
   - 源 inode 不能是目录，不能是 Append/Immutable。
   - `tx.get(entryKey(parent, name))`：目标名字不能已存在（CaseInsensi 时再 resolveCase）。

2. **更新父目录 mtime/ctime**（按 SkipDirMtime 策略，可能跳过）。

3. **硬链接核心**：
   - `oldParent := iattr.Parent`
   - `iattr.Parent = 0`（多父，交给 parentKey）
   - `iattr.Ctime = now`, `iattr.Nlink++`
   - `tx.set(entryKey(parent, name), packEntry(iattr.Typ, inode))`：新增目录项。
   - `tx.set(inodeKey(inode), marshal(&iattr))`：写回 inode 属性。
   - 若 `oldParent > 0`：`tx.incrBy(parentKey(inode, oldParent), 1)`（原父目录下该 inode 的引用 +1，因为原来单父时没有 parentKey，第一次变成多父要在旧父下补一条计数）。
   - `tx.incrBy(parentKey(inode, parent), 1)`：新父目录下该 inode 的引用 +1。

4. 若有 `attr != nil`，则回填 `*attr = iattr` 供上层返回。

**要点**：第一次从「单父」变为「多父」时，把原 Parent 写入 `parentKey(inode, oldParent)`；之后所有父目录都只通过 parentKey 维护。

### 4.2 Redis 引擎（`pkg/meta/redis.go`）

#### 4.2.1 parentKey 结构

```go
func (m *redisMeta) parentKey(inode Ino) string {
    return m.prefix + "p" + inode.String()
}
```

使用 Redis Hash：**key = parentKey(inode)**，**field = parent 的字符串**，**value = count**。即一个 inode 对应一个 Hash，里面存所有 parent 及其引用数。

#### 4.2.2 doLink 步骤（约 2518–2588 行）

逻辑与 TKV 对齐：

- MGet 取父目录和源 inode 的 attr；类型、权限、Immutable 等检查一致。
- `iattr.Parent = 0`, `iattr.Nlink++`, Ctime 更新。
- HGet 检查目标 name 是否已存在；CaseInsensi 时 resolveCase。
- Pipeline：HSet 写 entry；可选 Set 父目录 attr；Set 写 inode attr；`HIncrBy(parentKey(inode), oldParent.String(), 1)` 和 `HIncrBy(parentKey(inode), parent.String(), 1)`。

事务用 `m.inodeKey(parent), m.entryKey(parent), m.inodeKey(inode)` 等做 Watch。

### 4.3 SQL 引擎（`pkg/meta/sql.go`）

#### 4.3.1 无单独 parentKey 表

SQL 用 **edge 表** 表示「(parent, name) -> inode」。一个 inode 若被多条 edge 引用，自然就有多个 parent。因此不需要单独的 parent 表，**GetParents** 通过「查 edge 表中 Inode = inode 的所有行」得到 parent 及数量。

#### 4.3.2 doLink 步骤（约 2468–2546 行）

- 查父目录 node、目标 edge 是否存在；父须为目录且非 trash；Access、Immutable 检查。
- 查源 inode 的 node，类型不能是目录，不能 Append/Immutable。
- `n.Parent = 0`, `n.Nlink++`, 更新 Ctime。
- `mustInsert(s, &edge{Parent: parent, Name: []byte(name), Inode: inode, Type: n.Type})`：插入新 edge。
- `Update(&n, node{Inode: inode})` 只更新 `nlink, ctime, ctimensec, parent`。
- 若需要则更新父目录 mtime/ctime。

**GetParents**（约 3272–3286 行）：`Find(&rows, &edge{Inode: inode})`，然后对 `row.Parent` 计数得到 `map[Ino]int`。

---

## 五、Unlink 与硬链接

硬链接的删除就是普通 **Unlink**：删除一条 `(parent, name)` 的 edge，并让该 inode 的 Nlink 减 1。

### 5.1 baseMeta.Unlink（base.go 约 1651–1678 行）

- 调用 `m.en.doUnlink(ctx, parent, name, &attr, ...)`。
- 成功后根据 attr 更新目录统计、目录配额、用户组配额。若 `attr.Nlink > 0` 且是文件，只减 inode 数（`updateUserGroupQuota(..., 0, -1)`）；若 Nlink 归零则同时减空间。

### 5.2 TKV doUnlink 中与硬链接相关的部分（tkv.go 约 1365–1436 行）

- 删除 `entryKey(parent, name)`。
- 若 **attr.Nlink > 0**（还有其它硬链接）：
  - 更新 inode attr（Nlink、Ctime 等）；若进回收站则写 trash entry。
  - 若 **attr.Parent == 0**（多父）：`tx.incrBy(parentKey(inode, parent), -1)`，减少当前 parent 下的引用。
- 若 **attr.Nlink == 0**：
  - 删除 inode、xattr、symlink 等；若是文件且未打开则入 delfile 等。
  - 若 **attr.Parent == 0**：`tx.deleteKeys(fmtKey("A", inode, "P"))`，删除该 inode 下所有 parentKey。

Redis 逻辑类似：Parent==0 时对 `parentKey(inode)` 的 Hash 里对应 parent 做 HIncrBy -1，或 Del 整个 key。

---

## 六、GetParents：多父查询

当 inode 有多个父目录（硬链接）时，需要枚举「所有 parent 及在该 parent 下的链接数」。

### 6.1 baseMeta.GetParents（base.go 约 2217–2230 行）

```go
func (m *baseMeta) GetParents(ctx Context, inode Ino) map[Ino]int {
    // RootInode/TrashInode 特殊处理
    if st := m.GetAttr(ctx, inode, &attr); st != 0 { return nil }
    if attr.Parent > 0 {
        return map[Ino]int{attr.Parent: 1}  // 单父
    }
    return m.en.doGetParents(ctx, inode)   // 多父：查 parentKey 或 edge
}
```

### 6.2 各引擎 doGetParents

- **TKV**（约 2496–2510 行）：扫描前缀 `A{inode}P` 的 key，解析出 parent 和 count（parseCounter(v)），返回 `map[Ino]int`。
- **Redis**（约 3063–3078 行）：`HGetAll(parentKey(inode))`，对每个 field（parent 字符串）解析计数。
- **SQL**（约 3272–3286 行）：`Find(&rows, &edge{Inode: inode})`，按 `row.Parent` 计数。

用于配额、目录统计、回收站等需要「该 inode 属于哪些目录」的场景。

---

## 七、Rename 与硬链接

Rename 可能移动的是「某 inode 的其中一个链接」。若该 inode 有多个父（Parent==0），则需要在 parentKey 中把「原父」的计数减 1，「新父」的计数加 1。若 Rename 后该 inode 只剩一个父，部分实现会把 Parent 设回该父并清空 parentKey（代码里 TKV/Redis 的 doRename 中有 `iattr.Parent = parentDst` 等，表示目标只有一个父时简化存储）。具体以各引擎 doRename 实现为准。

---

## 八、配额与回收站小结

- **创建硬链接**：目录 inode 数 +1，用户/组 inode 数 +1，**空间不变**。
- **删除硬链接**：若 Nlink 仍 > 0，只减 inode 数，不减空间；若 Nlink 归 0，才减空间并可能进入 delfile/回收站逻辑。
- **回收站**：若 unlink 时进 trash，且 attr.Parent==0，会在 parentKey 中记录 trash 目录的引用（见 doUnlink 中 trash 分支）。

---

## 九、小结表

| 项目           | 说明 |
|----------------|------|
| **Nlink**      | 链接数；Create 为 1，Link +1，Unlink -1；0 时回收 inode。 |
| **Parent**     | 单父时存唯一父 inode；有硬链接后置 0，由 parentKey/edge 维护多父。 |
| **parentKey**  | TKV: `A{inode}P{parent}`→count；Redis: Hash key=inode, field=parent, value=count；SQL: 无，用 edge 表反查。 |
| **GetParents**| Parent>0 返回单元素 map；否则 doGetParents 扫 parentKey 或 edge。 |
| **配额**       | 硬链接只增加 inode 计数，不增加空间；删除链接时仅 Nlink 归零才减空间。 |

以上即为 JuiceFS 硬链接的完整实现原理与代码路径分析。
