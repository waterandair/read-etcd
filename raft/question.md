## raft 查询 term 的问题
raftLog 在查询 term时， 先从 unstable 中查，没有的话再去 Storage 中查，但是如果索引在 unstable 或 Storage 的快照中， 就不能查找，返回了 0 

