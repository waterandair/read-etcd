#### 各类消息的处理
##### MsgHup 本地消息，发起选举 
`—— 这是集群生命周期中的第一个消息，就像婴儿的第一声啼哭一样美妙`
集群启动一段时间后，会有一个 Follower 节点的选举计时器超时（electionTimeout），此时该节点会创建一个 Term 值为 0 （代表本地消息）的 MsgHup 并将其发送给消息处理函数，
根据当前节点是否开启 PreVote 模式进行下一步处理： 如果开启了 PreVote 模式，会向其他节点发送 MsgPreVote 请求； 如果没有开启 PreVote 模式，会直接向其他节点发送 MsgVote 请求.  

##### MsgPreVote 发起选举投票请求消息
Follower 节点会收到此类消息：  
首先检测这个消息是否是 Leader 节点迁移时发出的及其他合法性检测，然后决定当前节点是否参与此次选举，向消息的发送节点投票
##### MsgPreVoteResp 发起预选举投票的响应消息
PreCandidate 节点会收到此类消息：
当 PreCandidate 节点收到半数以上的投票时，会将自己的状态切换为 Candidate 并发起正式的选举
##### MsgVote 发起选举投票请求消息  
Follower 节点会收到此类消息：  
检测当前节点是和否投票及发送过了 MsgVoteResp 消息，重置当前节点的选举超时计时器，并更新 raft.Vote 字段。
##### MsgVoteResp 选举投票响应消息
Candidate 节点会收到此类消息：  
首先检测当前节点是否通过了选举，即是否收到了半数以上的选票，如果是，则将当前节点切换成 Leader 状态，之后像集群中其他节点发送消息。

##### MsgApp 日志复制请求消息
Follower 节点收到此类消息：
1. 重置选举超时计算器，防止发起新一轮选举
2. 记录当前集群的 leader 节点 ID
3. 判断 MsgApp 消息中保存的 Index（表示 MsgApp 携带的日志的前一条日志的索引） 值是否小于当前节点 raftLog 的最后一个提交的日志索引，如果是，则返回 raftLog.committed, 提示leader 节点从正确的索引位置发送日志
4. 尝试将 MsgApp 携带的 entries 追加到 raftLog 中，根据追加结果返回相应 MsgAppResp 消息


Candidate 节点收到此类消息：
1. 将当前节点的状态转换为 Follower
2. 执行与上述 Follower 节点的逻辑

##### MsgAppResp 日志复制的响应消息
todo

##### MsgBeat 心跳消息和 MsgCheckQuorum 探活消息
这两种消息的 Term 值都为 0， 都属于本地消息。  

Leader 节点除了向集群中其他 Follower 发送 MsgApp 消息，还会向这些 Follower 节点发送 MsgHeartbeat 消息。 Leader 会发送一个 MsgBeat 的本地消息，
处理这个消息的过程就是向 Follower 节点广播 MsgHeartbeat ， 主要作用的心跳探活，
当 follower 节点收到此消息，就会重置自己的选举超时器（election_timeout），防止 Follower 节点发起选举.  


##### MsgHeartbeat 和 MsgHeartbeatResp 消息


##### MsgProp 客户端写请求的消息
只有 Leader 节点可以真正的处理此类消息， Candidate 节点会直接忽略掉此类消息，Follower 会将此类消息转发给当前集群中的 Leader

##### MsgReadIndex， MsgReadIndexResp， 客户端读消息和响应
客户端的读请求需要读到最新的已提交的数据，不能读到老数据。Leader 节点保存了整个集群中最新的数据，但在网络分区的场景下，旧的 Leader 节点就可能返回旧数据。  

MsgReadIndex 类型的消息就是用来解决这个问题的：  
当客户端只读请求发送到 Leader 节点后，Leader 会将请求中的编号记录下来，在返回数据给客户端之前，Leader 节点需要先确定自定是否依然是当前集群的 Leader 节点，  
确定之后，就可以等待 Leader 节点的提交位置（raftLog.committed） 到达或者超过只读请求的编号即可向客户端返回响应。  

只读请求有两种模式，分别是 ReadOnlySafe 和 ReadOnlyLeaseBased，ReadOnlySafe 模式是 etcd 作者推荐的模式，在这种模式下的只读请求处理
，不会受节点之间时钟差异和网络分区的影响。  


##### MsgSnap 消息
Leader 节点尝试向集群中的 Follower 节点发送 MsgApp 消息时，如果查找不到待发送的 Entry 记录（即该 Follower 节点对应的 Process.Next 指定的Entry记录），
则会尝试通过 MsgSnap 消息将快照数据发送个到 Follower 节点，Follower 节点之后会通过快照数据恢复其自身状态，从而可以与 Leader 节点进行正常的 Entry 记录复制。  

当 Follower 节点宕机时间比较长时，就可能会出现发送 MsgSnap 消息的场景

##### MsgTransferLeader 和 MsgTimeoutNow 
MsgTransferLeader 是当 Leader 节点迁移时，Leader 节点的本地消息，Leader 节点对 MsgTransferLeader 处理中验证是否可以进行迁移，如果可以迁移，会向目标 Follower 
发送 MsgTimeoutNow 消息。   
Leader 在进行迁移时，会停止处理客户端的 MsgProp 请求

#### 一条 Entry 的处理流程
- 客户端向集群发起一次请求，请求中封装的 Entry 会先被 etcd-raft 模块进行处理，etcd-raft 会先将 Entry 记录保存到 raftLog.unstable 中。
- etcd-raft 模块将该 Entry 记录封装到前面介绍的 Ready 实例中，返回给上层模块进行持久化
- 上层模块收到带持久化的 Entry 记录后，会先将其记录到 WAL 日志文件中，然后进行持久化操作，最后通知 etcd-raft 模块进行处理
- 此时，etcd-raft 模块就会将该 Entry 记录从 unstable 移动到 storage 中保存。
- Entry 记录被复制到集群中的半数以上节点时，该 Entry 记录会被 Leader 节点确定为已提交， 并封装进 Ready 实例返回给上层模块
- 此时，上层模块即可将该 Ready 实例中携带的待应用的 Entry 记录应用到状态机中

































