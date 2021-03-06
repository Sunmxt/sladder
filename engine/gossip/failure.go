package gossip

import (
	"container/heap"
	"sync"
	"time"

	"github.com/crossmesh/sladder"
	"github.com/crossmesh/sladder/engine/gossip/pb"
	"github.com/crossmesh/sladder/proto"
	"github.com/golang/protobuf/ptypes"
)

type suspection struct {
	notAfter   time.Time
	node       *sladder.Node
	queueIndex int
}

type suspectionQueue []*suspection

func (q suspectionQueue) Len() int           { return len(q) }
func (q suspectionQueue) Less(i, j int) bool { return q[i].notAfter.Before(q[j].notAfter) }
func (q suspectionQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].queueIndex = i
	q[j].queueIndex = j
}
func (q *suspectionQueue) Push(x interface{}) {
	s := x.(*suspection)
	s.queueIndex = q.Len()
	*q = append(*q, s)
}
func (q *suspectionQueue) Pop() (x interface{}) {
	old, n := *q, q.Len()
	x = old[n-1]
	*q = old[:n-1]
	return x
}

type proxyPingRequest struct {
	target []string
	origin []string
	id     uint64
}

type pingContext struct {
	lock     sync.Mutex
	id       uint64
	start    time.Time
	proxyFor []*proxyPingRequest

	indirects uint32
}

func (e *EngineInstance) goDetectFailure() {
	if !e.disableFailureDetect {
		e.tickGossipPeriodGo(func(deadline time.Time) {
			e.DetectFailure()
		})
	}

	if !e.disableClearSuspections {
		e.tickGossipPeriodGo(func(deadline time.Time) {
			e.ClearSuspections()
		})
	}

}

func (e *EngineInstance) _clearNodeFromFailureDetector(node *sladder.Node) {
	// remove node from all failure detector fields.
	if ctx, exist := e.inPing[node]; exist {

		minc := &FailureDetectorMetricIncrement{}

		ctx.lock.Lock()
		// update metrics.
		if ctx.indirects > 0 {
			minc.PingIndirect -= ctx.indirects
		}
		if numOfProxied := len(ctx.proxyFor); numOfProxied > 0 {
			minc.ProxyFailure += uint64(numOfProxied)
		}
		minc.Ping--
		ctx.lock.Unlock()

		e.Metrics.FailureDetector.ApplyIncrement(minc)

		delete(e.inPing, node)
	}
	delete(e.roundTrips, node)
	if s, _ := e.suspectionNodeIndex[node]; s != nil {
		heap.Remove(&e.suspectionQueue, s.queueIndex)
		delete(e.suspectionNodeIndex, node)
	}
}

func (e *EngineInstance) updateEngineRelatedFields(t *sladder.Transaction,
	isEngineTxn bool,
	ops []*sladder.TransactionOperation) (accepted bool, err error) {

	nodes := make(map[*sladder.Node]int)
	for idx, op := range ops {
		if op.Txn == nil {
			if op.NodeExists == op.NodePastExists {
				continue
			}
			nodes[op.Node] = idx
		} else if op.Key == e.swimTagKey {
			if _, exists := nodes[op.Node]; !exists {
				nodes[op.Node] = -1
			}
		}
	}

	type oneRegionParam struct {
		node   *sladder.Node
		region string
	}

	type regionUpdation struct {
		node     *sladder.Node
		old, new string
	}

	type stateUpdation struct {
		node *sladder.Node
		new  SWIMState
	}

	var regionOp struct {
		insertions, deletions []*oneRegionParam
		updations             []*regionUpdation
	}
	selfRegionUpdated, newRegion := false, ""
	var stateUpdates []*stateUpdation

	minc := &StateMetricIncrement{}
	addStateMetricsByState := func(state SWIMState, n uint32) {
		switch state {
		case ALIVE:
			minc.Alive += n
		case SUSPECTED:
			minc.Suspected += n
		case DEAD:
			minc.Dead += n
		case LEFT:
			minc.Left += n
		}
	}

	for node, idx := range nodes {
		rtx, err := t.KV(node, e.swimTagKey)
		if err != nil {
			e.log.Errorf("engine cannot trace swim tag. (err = \"%v\")", err)
			return false, err
		}
		tag, oldTag := rtx.(*SWIMTagTxn), &SWIMTag{}
		if err := oldTag.Decode(tag.Before()); err != nil {
			e.log.Errorf("failed to decode old SWIM tag. (err = \"%v\")", err)
			return false, err
		}

		if idx >= 0 { // node op.
			op := ops[idx]
			param := &oneRegionParam{
				node: node, region: oldTag.Region,
			}
			if op.NodeExists { // insert.
				regionOp.insertions = append(regionOp.insertions, param)

				addStateMetricsByState(tag.State(), 1) // state metrics incremental.

			} else { // deletion.
				regionOp.deletions = append(regionOp.deletions, param)

				addStateMetricsByState(tag.State(), 0xFFFFFFFF) // state metrics incremental.
			}
		} else if !rtx.Updated() { // tag not updated.
			continue
		} else {
			if new, old := tag.Region(), oldTag.Region; new != old {
				regionOp.updations = append(regionOp.updations, &regionUpdation{
					node: node, old: old, new: new,
				})
				if self := e.cluster.Self(); node == self {
					selfRegionUpdated, newRegion = true, new
				}
			}
			if new, old := tag.State(), oldTag.State; new != old {
				stateUpdates = append(stateUpdates, &stateUpdation{
					node: node, new: new,
				})

				addStateMetricsByState(old, 0xFFFFFFFF) // state metrics incremental.
				addStateMetricsByState(new, 1)          // state metrics incremental.
			}
		}
	}

	if len(nodes) < 1 {
		return true, nil
	}

	t.DeferOnCommit(func() {
		e.Metrics.State.ApplyIncrement(minc)

		if len(regionOp.insertions)+
			len(regionOp.deletions)+
			len(regionOp.updations)+
			len(stateUpdates) <= 0 && !selfRegionUpdated {
			return
		}

		e.lock.Lock()
		defer e.lock.Unlock()

		if selfRegionUpdated {
			e.region = newRegion
		}
		for _, insertion := range regionOp.insertions {
			e.insertToRegion(insertion.region, insertion.node)
		}
		for _, deletion := range regionOp.deletions {
			e.removeFromRegion(deletion.region, deletion.node, -1)
			e._clearNodeFromFailureDetector(deletion.node)
			e._clearNodeFromSyncer(deletion.node)
		}
		for _, updation := range regionOp.updations {
			e.updateRegion(updation.old, updation.new, updation.node)
		}
		for _, updation := range stateUpdates {
			e._traceSuspections(updation.node, updation.new)
		}
	})

	return true, nil
}

func (e *EngineInstance) _traceSuspections(node *sladder.Node, new SWIMState) {
	// trace suspection states.
	s, suspected := e.suspectionNodeIndex[node]
	if new != SUSPECTED {
		if suspected {
			heap.Remove(&e.suspectionQueue, s.queueIndex)
			delete(e.suspectionNodeIndex, node)
		}
	} else if !suspected {
		s = &suspection{
			notAfter: time.Now().Add(e.getGossipPeriod() * 10),
			node:     node,
		}
		heap.Push(&e.suspectionQueue, s)
		e.suspectionNodeIndex[node] = s
	}
}

func (e *EngineInstance) setLeavingNodeTimeout(node *leavingNode) {
	time.AfterFunc(e.getGossipPeriod()*30, func() { e.untraceLeaveingNode(node) })
}

func (e *EngineInstance) untraceLeaveingNode(node *leavingNode) {
	e.lock.Lock()
	defer e.lock.Unlock()
	e._untraceLeavingNode(node)
}

func (e *EngineInstance) _untraceLeavingNode(node *leavingNode) {
	var removeIdx []int
	for _, name := range node.names {
		idx, exists := e.leaveingNodeNameIndex[name]
		if !exists || e.leavingNodes[idx] != node {
			continue
		}
		removeIdx = append(removeIdx, idx)
	}

	if len(removeIdx) > 0 {
		e._removeLeavingNode(removeIdx...)
	}
}

func (e *EngineInstance) traceLeavingNode(node *leavingNode) {
	if node == nil {
		return
	}
	if len(node.names) < 1 { // ignore anonymous node.
		return
	}

	e.lock.Lock()
	defer e.lock.Unlock()

	// clear collision.
	var removeIdx []int
	for _, name := range node.names {
		idx, exists := e.leaveingNodeNameIndex[name]
		if !exists {
			continue
		}
		removeIdx = append(removeIdx, idx)
	}
	if len(removeIdx) > 0 {
		e._removeLeavingNode(removeIdx...)
	}

	node.tagIdx = -1
	for idx, entry := range node.snapshot.Kvs {
		if entry.Key == e.swimTagKey {
			node.tagIdx = idx
		}
	}
	newIdx := len(e.leavingNodes)
	e.leavingNodes = append(e.leavingNodes, node)
	for _, name := range node.names {
		e.leaveingNodeNameIndex[name] = newIdx
	}

	// set timeout.
	e.setLeavingNodeTimeout(node)
}

func (e *EngineInstance) clearDeads() {
	if err := e.cluster.Txn(func(t *sladder.Transaction) (changed bool) {
		// mark txn internal.
		e.innerTxnIDs.Store(t.ID(), struct{}{})

		changed = false

		e.lock.Lock()
		defer e.lock.Unlock()

		minRegionPeer := int(e.minRegionPeer)

		for _, nodes := range e.withRegion {
			curNum := len(nodes)
			if minRegionPeer >= curNum {
				continue
			}
			allows := curNum - minRegionPeer

			for node := range nodes {
				if allows < 1 {
					break
				}

				rtx, err := t.KV(node, e.swimTagKey)
				if err != nil {
					e.log.Warn("cannot get SWIM tag of node %v. skip. (err = \"%v\")", t.Names(node), err)
					continue
				}
				tag := rtx.(*SWIMTagTxn)
				if tag.State() == DEAD && allows > 0 {
					t.RemoveNode(node)
					changed = true
					allows--
				}
			}
		}

		return
	}, sladder.MembershipModification()); err != nil {
		e.log.Error("failed to clear dead nodes. got transaction failure. retry later. (err = \"%v\")", err)
		e.delayClearDeads(time.Second * 5)
	}
}

func (e *EngineInstance) delayClearDeads(delay time.Duration) {
	// TODO(xutao): submit to cluster's job queue instead.
	e.arbiter.Go(func() {
		if delay > 0 {
			time.Sleep(delay)
		}
		e.clearDeads()
	})
}

func (e *EngineInstance) removeIfDeadOrLeft(node *sladder.Node, tag *SWIMTag) {
	if tag.State != DEAD && tag.State != LEFT {
		return
	}
	if node == e.cluster.Self() { // never remove self here.
		return
	}

	var leaving *leavingNode

	if err := e.cluster.Txn(func(t *sladder.Transaction) bool {
		e.lock.Lock()
		defer e.lock.Unlock()

		// mark txn internal.
		e.innerTxnIDs.Store(t.ID(), struct{}{})

		removed := false
		nodeSet, exists := e.withRegion[tag.Region]
		if !exists {
			// should not reach this.
			e.log.Error("[BUG!] a node not traced by region map.")
			removed = true
		} else if _, inNodeSet := nodeSet[node]; !inNodeSet {
			// should not reach this.
			e.log.Error("[BUG!] a node not in region node set.")
			removed = true
		} else if tag.State == LEFT ||
			uint(len(nodeSet)) > e.minRegionPeer { // limitation of region peer count.
			removed = true
		}

		if !removed {
			return false
		}

		leaving = &leavingNode{
			names:    t.Names(node),
			snapshot: &proto.Node{},
		}
		t.ReadNodeSnapshot(node, leaving.snapshot)
		t.RemoveNode(node) // TODO(xutao): report bug when an error returned.

		return true
	}, sladder.MembershipModification()); err != nil {
		e.log.Warnf("failed to remove a %v node. commit failure occurs. (err = %v) {node = %v}", tag.State, err, node.PrintableName())
		return
	}

	if leaving != nil {
		e.traceLeavingNode(leaving)
	}
}

// ClearSuspections clears all expired suspection.
func (e *EngineInstance) ClearSuspections() {
	if !e.arbiter.ShouldRun() {
		return
	}

	e.lock.Lock()

	if e.suspectionQueue.Len() < 1 {
		// no suspection.
		e.lock.Unlock()
		return
	}
	now := time.Now()

	var deads []*sladder.Node

	if !now.After(e.suspectionQueue[0].notAfter) {
		e.lock.Unlock()
		// no expired one.
		return
	}

	for e.suspectionQueue.Len() > 0 { // find all expired suspection.
		s := e.suspectionQueue[0]
		if !now.After(s.notAfter) {
			break
		}
		deads = append(deads, s.node)

		heap.Pop(&e.suspectionQueue)
		delete(e.suspectionNodeIndex, s.node)
	}

	e.lock.Unlock()

	if len(deads) > 0 {
		idx := 0
		for ; idx < len(deads); idx++ {
			node := deads[idx]

			if err := e.cluster.Txn(func(t *sladder.Transaction) bool {
				// mark txn internal.
				e.innerTxnIDs.Store(t.ID(), struct{}{})

				// claim dead.
				rtx, err := t.KV(node, e.swimTagKey)
				if err != nil {
					e.log.Errorf("get key-value in claiming dead transaction failure. {node = %v} (err = %v)", node.PrintableName(), err.Error())
					return false
				}
				tag := rtx.(*SWIMTagTxn)
				return tag.ClaimDead()
			}); err != nil {
				e.log.Errorf("failed to commit dead claiming transaction. {node = %v} (err = %v)", node.PrintableName(), err.Error())
				break
			}
		}
	}
}

// DetectFailure does one failure detection process.
func (e *EngineInstance) DetectFailure() {
	if !e.arbiter.ShouldRun() {
		return
	}

	nodes := e.selectRandomNodes(e.getGossipFanout(), true)
	if len(nodes) < 1 {
		return
	}

	for _, node := range nodes {
		e.ping(node, nil)
	}
}

func (e *EngineInstance) estimatedRoundTrip(node *sladder.Node) time.Duration {
	return e.getGossipPeriod()
	//rtt, estimated := e.roundTrips[node]
	//if !estimated || rtt < 1 {
	//	return e.getGossipPeriod()
	//}
	//return rtt
}

func (e *EngineInstance) processFailureDetectionProto(from []string, msg *pb.GossipMessage) {
	switch msg.Type {
	case pb.GossipMessage_Ack:
		ack := &pb.Ack{}
		if err := ptypes.UnmarshalAny(msg.Body, ack); err != nil {
			e.log.Error("invalid ack body, got " + err.Error())
			break
		}
		e.onPingAck(from, ack)

	case pb.GossipMessage_Ping:
		ping := &pb.Ping{}
		if err := ptypes.UnmarshalAny(msg.Body, ping); err != nil {
			e.log.Error("invalid ping body, got " + err.Error())
			break
		}
		e.onPing(from, ping)

	case pb.GossipMessage_PingReq:
		pingReq := &pb.PingReq{}
		if err := ptypes.UnmarshalAny(msg.Body, pingReq); err != nil {
			e.log.Error("invalid ping-req body, got " + err.Error())
			break
		}
		e.onPingReq(from, pingReq)
	}
}

func (e *EngineInstance) ping(node *sladder.Node, proxyReq *proxyPingRequest) {
	if node == nil {
		return
	}

	names := node.Names()

	minc := &FailureDetectorMetricIncrement{}
	defer e.Metrics.FailureDetector.ApplyIncrement(minc)

	e.lock.Lock()
	defer e.lock.Unlock()

	pingCtx, _ := e.inPing[node]

	if pingCtx == nil { // not in progres.
		id := e._generateMessageID()
		defer e.sendProto(names, &pb.Ping{
			Id: id,
		})

		pingCtx = &pingContext{
			id:    id,
			start: time.Now(),
		}

		// after a ping timeout, a ping-req may be sent.
		time.AfterFunc(e.estimatedRoundTrip(node)*2, func() {
			e.pingTimeoutEvent <- node
		})

		e.inPing[node] = pingCtx

		minc.Ping++
	}

	if proxyReq != nil {
		pingCtx.lock.Lock()
		pingCtx.proxyFor = append(pingCtx.proxyFor, proxyReq)
		pingCtx.lock.Unlock()

		minc.ProxyPing++
	}
}

func (e *EngineInstance) onPingAck(from []string, msg *pb.Ack) {
	var node *sladder.Node

	if len(msg.NamesProxyFor) < 1 {
		node = e.cluster.MostPossibleNode(from)
	} else {
		node = e.cluster.MostPossibleNode(msg.NamesProxyFor)
	}

	if node == nil {
		return
	}

	minc := &FailureDetectorMetricIncrement{}
	defer e.Metrics.FailureDetector.ApplyIncrement(minc)

	e.lock.Lock()
	defer e.lock.Unlock()

	pingCtx, _ := e.inPing[node]
	if pingCtx == nil {
		return
	}

	pingCtx.lock.Lock()
	defer pingCtx.lock.Unlock()

	// save estimated round-trip time.
	rtt := time.Now().Sub(pingCtx.start)
	e.roundTrips[node] = rtt

	if numOfProxied := len(pingCtx.proxyFor); numOfProxied > 0 {
		for _, pingReq := range pingCtx.proxyFor {
			// ack for all related ping-req.
			e.sendProto(pingReq.origin, &pb.Ack{
				NamesProxyFor: pingReq.target,
				Id:            pingReq.id,
			})
		}

		minc.ProxySuccess += uint64(numOfProxied)
	}
	minc.Ping--
	minc.Success++

	delete(e.inPing, node)
}

func (e *EngineInstance) onPing(from []string, msg *pb.Ping) {
	if msg == nil {
		return
	}

	// ack.
	e.sendProto(from, &pb.Ack{
		Id: msg.Id,
	})
}

func (e *EngineInstance) processPingTimeout(node *sladder.Node) {
	if node == nil {
		return
	}

	e.lock.RLock()
	pingCtx, _ := e.inPing[node]
	e.lock.RUnlock()

	if pingCtx == nil {
		return
	}

	req := &pb.PingReq{
		Id:   pingCtx.id,
		Name: node.Names(),
	}
	minc := &FailureDetectorMetricIncrement{}
	defer e.Metrics.FailureDetector.ApplyIncrement(minc)

	timeout := time.Duration(0)
	pingCtx.lock.Lock()
	for _, proxy := range e.selectRandomNodes(e.getPingProxiesCount(), true) {
		if proxy == node {
			continue
		}

		e.sendProto(proxy.Names(), req) // ping-req.
		pingCtx.indirects++
		minc.PingIndirect++

		// find minimal proxier round-trip.
		if rtt := e.estimatedRoundTrip(proxy); timeout < 1 || rtt < timeout {
			timeout = rtt
		}
	}
	pingCtx.lock.Unlock()

	if gossipPeriod := e.getGossipPeriod(); gossipPeriod > timeout {
		timeout = gossipPeriod
	}

	time.AfterFunc(timeout*time.Duration(e.getMinPingReqTimeoutTimes()), func() {
		e.pingReqTimeoutEvent <- node
	})
}

func (e *EngineInstance) processPingReqTimeout(node *sladder.Node) {
	e.lock.RLock()
	pingCtx, _ := e.inPing[node]
	e.lock.RUnlock()

	if pingCtx == nil {
		return
	}

	// raise suspection.
	if err := e.cluster.Txn(func(t *sladder.Transaction) bool {
		// mark txn internal.
		e.innerTxnIDs.Store(t.ID(), struct{}{})

		{
			rtx, err := t.KV(node, e.swimTagKey)
			if err != nil {
				e.log.Error("cannot get KV Txn when claiming suspection, got " + err.Error())
				return false
			}
			tag := rtx.(*SWIMTagTxn)
			tag.ClaimSuspected()
		}
		return true
	}); err != nil {
		e.log.Error("transaction commit failure when claiming suspection, got " + err.Error())
	}

	minc := &FailureDetectorMetricIncrement{}
	defer e.Metrics.FailureDetector.ApplyIncrement(minc)

	e.lock.Lock()
	if _, exist := e.inPing[node]; exist {
		delete(e.inPing, node)
		minc.Ping--
		minc.Failure++
	}
	e.lock.Unlock()

	pingCtx.lock.Lock()
	if pingCtx.indirects > 0 {
		minc.PingIndirect -= pingCtx.indirects
	}
	pingCtx.lock.Unlock()
}

func (e *EngineInstance) onPingReq(from []string, msg *pb.PingReq) {
	if msg == nil || len(msg.Name) < 1 {
		return
	}

	node := e.cluster.MostPossibleNode(msg.Name)
	if node == nil {
		minc := &FailureDetectorMetricIncrement{}
		minc.ProxyFailure++
		e.Metrics.FailureDetector.ApplyIncrement(minc)
		return
	}

	e.ping(node, &proxyPingRequest{
		origin: from,
		target: msg.Name,
		id:     msg.Id,
	}) // proxy ping.
}
