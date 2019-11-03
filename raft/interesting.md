# 代码中一些有趣巧妙的设计或关键部分
## Storage 接口中的 Entrys 预留第一个位置

Entries 数组的第一个位置预留给快照的最后一个 Entry.  

这样做有两个好处:  
- 第一方便进行有无快照的判断,只要判断第一个Entry的索引值和任期值是否为 0 就可以  
- 第二在完成一次快照后的第一次请求需要对快照进行一致性检查,也可以很容易的从 entries 的第一个位置中取出快照中最后一个 entry 的信息.

## MemoryStorage 结构的 Append 函数

从函数中的逻辑, 可以看出, 当前节点接收日志时, 不会已经覆盖快照的索引; 如果日志请求中包含当前 Storage 存储的 entries 中的索引, 那么它将覆盖 Storage 中相同索引位置的 Entry

## 极致的资源占用来自对细节孜孜不倦的执着, 看 etcd 如何优化动态数组

`/raft/log_unstable.go` 中的 `unstable` 结构体中维护了一些不稳定的数据, 就拿 `entries slice` 来说, 它存储了来自海量请求的 entry 记录, 
同时丢弃已经存入 Storage 的 entry 记录, 所以这个 slice 是在动态变化的.   

在 golang 中对一个 slice 进行 append 操作, 会使它的容量 cap 持续翻倍增长,才一些情况下, cap 会远大于 len.  

看如下代码: 

```
func main() {
	var arr []uint64

	arr = append(arr, 1)
	arr = append(arr, 1)
	arr = append(arr, 1)
	arr = append(arr, 1)
	arr = append(arr, 1)

	fmt.Println(len(arr), cap(arr))  // 5 8

	arr = arr[3:]

	fmt.Println(len(arr), cap(arr)) // 2 5
}

```

可以发现,在对 slice 进行一些增删操作后, cap 比 len 的两倍还大,当数据量达到一定量后, cap 多出来的部分就会白白占用内存,所以 etcd 对其进行了优化,
看 `func (u *unstable) shrinkEntriesArray()` 函数:

```
func (u *unstable) shrinkEntriesArray() {
	const lenMultiple = 2
	if len(u.entries) == 0 {
		u.entries = nil
	} else if len(u.entries)*lenMultiple < cap(u.entries) {
		newEntries := make([]pb.Entry, len(u.entries))
		copy(newEntries, u.entries)
		u.entries = newEntries
	}
}

```

但是这样做对资源和性能的影响究竟能有多大,会不会引起其他问题?比如会不会增加内存碎片?这个还有待验证.