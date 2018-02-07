package autopilot

import (
	"net"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
)

// Config couples all the items that that an autopilot agent needs to function.
// All items within the struct MUST be populated for the Agent to be able to
// carry out its duties.
type Config struct {
	// Self is the identity public key of the Lightning Network node that
	// is being driven by the agent. This is used to ensure that we don't
	// accidentally attempt to open a channel with ourselves.
	Self *btcec.PublicKey

	// Heuristic is an attachment heuristic which will govern to whom we
	// open channels to, and also what those channels look like in terms of
	// desired capacity. The Heuristic will take into account the current
	// state of the graph, our set of open channels, and the amount of
	// available funds when determining how channels are to be opened.
	// Additionally, a heuristic make also factor in extra-graph
	// information in order to make more pertinent recommendations.
	Heuristic AttachmentHeuristic

	// ChanController is an interface that is able to directly manage the
	// creation, closing and update of channels within the network.
	ChanController ChannelController

	PeerScanner       bool
	PeerScannerServer PeerScannerServer //cuz i didnt want to jam these methods into ChanController.
	// WalletBalance is a function closure that should return the current
	// available balance o the backing wallet.
	WalletBalance func() (btcutil.Amount, error)

	// Graph is an abstract channel graph that the Heuristic and the Agent
	// will use to make decisions w.r.t channel allocation and placement
	// within the graph.
	Graph ChannelGraph

	// TODO(roasbeef): add additional signals from fee rates and revenue of
	// currently opened channels
}

// channelState is a type that represents the set of active channels of the
// backing LN node that the Agent should be ware of. This type contains a few
// helper utility methods.
type channelState map[lnwire.ShortChannelID]Channel

// Channels returns a slice of all the active channels.
func (c channelState) Channels() []Channel {
	chans := make([]Channel, 0, len(c))
	for _, channel := range c {
		chans = append(chans, channel)
	}
	return chans
}

// ConnectedNodes returns the set of nodes we currently have a channel with.
// This information is needed as we want to avoid making repeated channels with
// any node.
func (c channelState) ConnectedNodes() map[NodeID]struct{} {
	nodes := make(map[NodeID]struct{})
	for _, channels := range c {
		nodes[channels.Node] = struct{}{}
	}

	// TODO(roasbeef): add outgoing, nodes, allow incoming and outgoing to
	// per node
	//  * only add node is chan as funding amt set

	return nodes
}

// Agent implements a closed-loop control system which seeks to autonomously
// optimize the allocation of satoshis within channels throughput the network's
// channel graph. An agent is configurable by swapping out different
// AttachmentHeuristic strategies. The agent uses external signals such as the
// wallet balance changing, or new channels being opened/closed for the local
// node as an indicator to re-examine its internal state, and the amount of
// available funds in order to make updated decisions w.r.t the channel graph.
// The Agent will automatically open, close, and splice in/out channel as
// necessary for it to step closer to its optimal state.
//
// TODO(roasbeef): prob re-word
type Agent struct {
	// Only to be used atomically.
	started uint32
	stopped uint32

	// cfg houses the configuration state of the Ant.
	cfg Config

	// chanState tracks the current set of open channels.
	chanState channelState

	// stateUpdates is a channel that any external state updates that may
	// affect the heuristics of the agent will be sent over.
	stateUpdates chan interface{}

	// totalBalance is the total number of satoshis the backing wallet is
	// known to control at any given instance. This value will be updated
	// when the agent receives external balance update signals.
	totalBalance btcutil.Amount

	quit chan struct{}
	wg   sync.WaitGroup

	connectableNodeAddressesLock sync.Mutex
	connectableNodeAddresses     *[]nodeAddrConnectability
	unconnectableNodes           *map[NodeID]struct{}
}

// NodeID is a simple type that holds a EC public key serialized in compressed
// format.
type NodeID [33]byte

// NewNodeID creates a new nodeID from a passed public key.
func NewNodeID(pub *btcec.PublicKey) NodeID {
	var n NodeID
	copy(n[:], pub.SerializeCompressed())
	return n
}

// New creates a new instance of the Agent instantiated using the passed
// configuration and initial channel state. The initial channel state slice
// should be populated with the set of Channels that are currently opened by
// the backing Lightning Node.
func New(cfg Config, initialState []Channel) (*Agent, error) {
	a := &Agent{
		cfg:          cfg,
		chanState:    make(map[lnwire.ShortChannelID]Channel),
		quit:         make(chan struct{}),
		stateUpdates: make(chan interface{}),
	}

	for _, c := range initialState {
		a.chanState[c.ChanID] = c
	}

	return a, nil
}

// Start starts the agent along with any goroutines it needs to perform its
// normal duties.
func (a *Agent) Start() error {
	if !atomic.CompareAndSwapUint32(&a.started, 0, 1) {
		return nil
	}

	log.Infof("Autopilot Agent starting")

	startingBalance, err := a.cfg.WalletBalance()
	if err != nil {
		return err
	}

	a.wg.Add(1)
	a.startPeerScanner()
	go a.controller(startingBalance)

	return nil
}

// Stop signals the Agent to gracefully shutdown. This function will block
// until all goroutines have exited.
func (a *Agent) Stop() error {
	if !atomic.CompareAndSwapUint32(&a.stopped, 0, 1) {
		return nil
	}

	log.Infof("Autopilot Agent stopping")

	close(a.quit)
	a.wg.Wait()

	return nil
}

func (a *Agent) WakeUp() {
	a.stateUpdates <- &noopWakeup{}
}

// balanceUpdate is a type of external state update that reflects an
// increase/decrease in the funds currently available to the wallet.
type balanceUpdate struct {
	balanceDelta btcutil.Amount
}

// chanOpenUpdate is a type of external state update the indicates a new
// channel has been opened, either by the Agent itself (within the main
// controller loop), or by an external user to the system.
type chanOpenUpdate struct {
	newChan Channel
}

// chanOpenFailureUpdate is a type of external state update that indicates
// a previous channel open failed, and that it might be possible to try again.
type chanOpenFailureUpdate struct{}

// heart beat
type noopWakeup struct{}

// chanCloseUpdate is a type of external state update that indicates that the
// backing Lightning Node has closed a previously open channel.
type chanCloseUpdate struct {
	closedChans []lnwire.ShortChannelID
}

// OnBalanceChange is a callback that should be executed each time the balance of
// the backing wallet changes.
func (a *Agent) OnBalanceChange(delta btcutil.Amount) {
	go func() {
		a.stateUpdates <- &balanceUpdate{
			balanceDelta: delta,
		}
	}()
}

// OnChannelOpen is a callback that should be executed each time a new channel
// is manually opened by the user or any system outside the autopilot agent.
func (a *Agent) OnChannelOpen(c Channel) {
	go func() {
		a.stateUpdates <- &chanOpenUpdate{
			newChan: c,
		}
	}()
}

// OnChannelOpenFailure is a callback that should be executed when the
// autopilot has attempted to open a channel, but failed. In this case we can
// retry channel creation with a different node.
func (a *Agent) OnChannelOpenFailure() {
	go func() {
		a.stateUpdates <- &chanOpenFailureUpdate{}
	}()
}

// OnChannelClose is a callback that should be executed each time a prior
// channel has been closed for any reason. This includes regular
// closes, force closes, and channel breaches.
func (a *Agent) OnChannelClose(closedChans ...lnwire.ShortChannelID) {
	go func() {
		a.stateUpdates <- &chanCloseUpdate{
			closedChans: closedChans,
		}
	}()
}

// mergeNodeMaps merges the Agent's set of nodes that it already has active
// channels open to, with the set of nodes that are pending new channels. This
// ensures that the Agent doesn't attempt to open any "duplicate" channels to
// the same node.
// plus merge unconnectable nodes as well.
func mergeNodeMaps(a map[NodeID]struct{}, b map[NodeID]struct{}, c map[NodeID]struct{},
	d map[NodeID]Channel) map[NodeID]struct{} {

	res := make(map[NodeID]struct{}, len(a)+len(b)+len(c)+len(d))
	for nodeID := range a {
		res[nodeID] = struct{}{}
	}
	for nodeID := range b {
		res[nodeID] = struct{}{}
	}
	for nodeID := range c {
		res[nodeID] = struct{}{}
	}
	for nodeID := range d {
		res[nodeID] = struct{}{}
	}

	return res
}

// mergeChanState merges the Agent's set of active channels, with the set of
// channels awaiting confirmation. This ensures that the agent doesn't go over
// the prescribed channel limit or fund allocation limit.
func mergeChanState(pendingChans map[NodeID]Channel,
	activeChans channelState) []Channel {

	numChans := len(pendingChans) + len(activeChans)
	totalChans := make([]Channel, 0, numChans)

	for _, activeChan := range activeChans.Channels() {
		totalChans = append(totalChans, activeChan)
	}
	for _, pendingChan := range pendingChans {
		totalChans = append(totalChans, pendingChan)
	}

	return totalChans
}

// controller implements the closed-loop control system of the Agent. The
// controller will make a decision w.r.t channel placement within the graph
// based on: it's current internal state of the set of active channels open,
// and external state changes as a result of decisions it  makes w.r.t channel
// allocation, or attributes affecting its control loop being updated by the
// backing Lightning Node.
func (a *Agent) controller(startingBalance btcutil.Amount) {
	defer a.wg.Done()

	// We'll start off by assigning our starting balance, and injecting
	// that amount as an initial wake up to the main controller goroutine.
	a.OnBalanceChange(startingBalance)

	// TODO(roasbeef): do we in fact need to maintain order?
	//  * use sync.Cond if so

	// failedNodes lists nodes that we've previously attempted to initiate
	// channels with, but didn't succeed.
	failedNodes := make(map[NodeID]struct{})

	// pendingOpens tracks the channels that we've requested to be
	// initiated, but haven't yet been confirmed as being fully opened.
	// This state is required as otherwise, we may go over our allotted
	// channel limit, or open multiple channels to the same node.
	pendingOpens := make(map[NodeID]Channel)
	var pendingMtx sync.Mutex
	var lastAwoken = time.Now().UTC()
	var minIdleSeconds = int64(10 * 60)
	// 10-minute wake up timer
	go func() {
		for {
			time.Sleep(time.Duration(minIdleSeconds) * time.Second)
			if int64((time.Since(lastAwoken) / time.Second)) < minIdleSeconds {
				//if something else woke us while we were sleeping then sleep again for now.
				continue
			}
			a.WakeUp()
		}
	}()
	for {
		select {
		// A new external signal has arrived. We'll use this to update
		// our internal state, then determine if we should trigger a
		// channel state modification (open/close, splice in/out).
		case signal := <-a.stateUpdates:
			log.Warnf("MSG Processing new external signal")
			checkIfMoreChansNeeded := false
			switch update := signal.(type) {
			// The balance of the backing wallet has changed, if
			// more funds are now available, we may attempt to open
			// up an additional channel, or splice in funds to an
			// existing one.
			case *balanceUpdate:
				log.Warnf("MSG Applying external balance state "+
					"update of: %v", update.balanceDelta)

				a.totalBalance += update.balanceDelta
				// checkIfMoreChansNeeded = true

			// The channel we tried to open previously failed for
			// whatever reason.
			case *chanOpenFailureUpdate:
				log.Warnf("MSG Retrying (or not??) after previous channel open failure.")

			// A new channel has been opened successfully. This was
			// either opened by the Agent, or an external system
			// that is able to drive the Lightning Node.
			case *chanOpenUpdate:
				log.Warnf("MSG New channel successfully opened, "+
					"updating state with: %v",
					spew.Sdump(update.newChan))

				newChan := update.newChan
				a.chanState[newChan.ChanID] = newChan

				pendingMtx.Lock()
				delete(pendingOpens, newChan.Node)
				pendingMtx.Unlock()

			// A channel has been closed, this may free up an
			// available slot, triggering a new channel update.
			case *chanCloseUpdate:
				log.Warnf("MSG Applying closed channel "+
					"updates: %v",
					spew.Sdump(update.closedChans))

				for _, closedChan := range update.closedChans {
					delete(a.chanState, closedChan)
				}
				// checkIfMoreChansNeeded = true

			case *noopWakeup:
				log.Warnf("MSG Noop Wake-up")
				lastAwoken = time.Now().UTC()
				checkIfMoreChansNeeded = true
			}

			if !checkIfMoreChansNeeded {
				log.Warnf("Not checking if we need to make more chans at the moment.")
				continue
			}

			// With all the updates applied, we'll obtain a set of
			// the current active channels (confirmed channels),
			// and also factor in our set of unconfirmed channels.
			confirmedChans := a.chanState
			pendingMtx.Lock()
			log.Debugf("P3nding channels: %v", spew.Sdump(pendingOpens))
			totalChans := mergeChanState(pendingOpens, confirmedChans)
			pendingMtx.Unlock()

			var unconnctableNodes *map[NodeID]struct{}
			if a.cfg.PeerScanner {
				unconnctableNodes = a.getUnconnectableNodesMap()
				if unconnctableNodes == nil {
					log.Warnf("Peer scanning is on and we do not think we know about any other peers to open channels with anyway so whats the point :(")
					continue
				}
			} else {
				unconnctableNodes = &map[NodeID]struct{}{}
			}
			log.Warnf("unconnectable nodes count: %d", len(*unconnctableNodes))
			// for unconnectableNodeID := range unconnctableNodes {

			// Now that we've updated our internal state, we'll
			// consult our channel attachment heuristic to
			// determine if we should open up any additional
			// channels or modify existing channels.
			log.Warnf("Determine if more chans needed ...")
			availableFunds, needMore := a.cfg.Heuristic.NeedMoreChans(
				totalChans, a.totalBalance,
			)
			if !needMore {
				log.Warnf("Doesn't think we need to open or modify any channels at this time.")
				continue
			}

			log.Warnf("Triggering attachment directive dispatch")

			// We're to attempt an attachment so we'll o obtain the
			// set of nodes that we currently have channels with so
			// we avoid duplicate edges.
			connectedNodes := a.chanState.ConnectedNodes()
			pendingMtx.Lock()
			nodesToSkip := mergeNodeMaps(connectedNodes, failedNodes, *unconnctableNodes, pendingOpens)
			pendingMtx.Unlock()

			// err := a.cfg.ChanController.CheckNodeConnectivity(a.cfg.Self, a.cfg.Graph)
			// continue

			// If we reach this point, then according to our
			// heuristic we should modify our channel state to tend
			// towards what it determines to the optimal state. So
			// we'll call Select to get a fresh batch of attachment
			// directives, passing in the amount of funds available
			// for us to use.
			log.Warnf("Select candidates with chosen heuristic ...")
			chanCandidates, err := a.cfg.Heuristic.Select(
				a.cfg.Self, a.cfg.Graph, availableFunds,
				nodesToSkip,
				totalChans,
			)
			if err != nil {
				log.Errorf("Unable to select candidates for "+
					"attachment: %v", err)
				continue
			}

			if len(chanCandidates) == 0 {
				log.Warnf("No eligible candidates to connect to")
				continue
			}
			if a.cfg.PeerScanner {
				//because peerscanner is going to leave us only with peers that we are very confident we will be able to actually open a channel to, we should limit the number of directives we try
				//to match the number of outputs the wallet has since each channel will need at least one output and we expect these all to succeed.
				availableOutputCount, err := a.cfg.PeerScannerServer.UnspentWitnessOutputCount()
				if err != nil {
					log.Warnf("Got err from trying to get a count of unspent witness outputs: %s", err.Error())
					continue
				}
				if len(chanCandidates) > availableOutputCount {
					chanCandidates = chanCandidates[:availableOutputCount]
					log.Warnf("Reducing directives down to the first %d to match number of outputs for chan creation.", len(chanCandidates))
				}
			}

			log.Warnf("Attempting to execute channel attachment "+
				"directives: %v", spew.Sdump(chanCandidates))

			// For each recommended attachment directive, we'll
			// launch a new goroutine to attempt to carry out the
			// directive. If any of these succeed, then we'll
			// receive a new state update, taking us back to the
			// top of our controller loop.
			pendingMtx.Lock()
			for _, chanCandidate := range chanCandidates {
				nID := NewNodeID(chanCandidate.PeerKey)
				pendingOpens[nID] = Channel{
					Capacity:    chanCandidate.ChanAmt,
					Node:        nID,
					IsInitiator: true,
				}

				go func(directive AttachmentDirective) {
					pub := directive.PeerKey
					err := a.cfg.ChanController.OpenChannel(
						directive.PeerKey,
						directive.ChanAmt,
						directive.Addrs,
					)
					if err != nil {
						log.Warnf("Unable to open "+
							"channel to %x of %v: %v",
							pub.SerializeCompressed(),
							directive.ChanAmt, err)

						// As the attempt failed, we'll
						// clear it from the set of
						// pending channels.
						pendingMtx.Lock()
						nID := NewNodeID(directive.PeerKey)
						delete(pendingOpens, nID)

						// Mark this node as failed so we don't
						// attempt it again.
						failedNodes[nID] = struct{}{}
						pendingMtx.Unlock()

						// Trigger the autopilot controller to
						// re-evaluate everything and possibly
						// retry with a different node.
						a.OnChannelOpenFailure()
					}

				}(chanCandidate)
			}
			pendingMtx.Unlock()

		// The agent has been signalled to exit, so we'll bail out
		// immediately.
		case <-a.quit:
			return
		}
	}
}

type nodeAddrConnectability struct {
	nID       NodeID
	node      Node
	addr      net.Addr
	count     int64
	startedAt time.Time
	started   bool
	finished  bool
	succeded  bool
}

func (a *Agent) checkNodeConnectivity() {
	nodesWithAddr := 0
	totalAddress := int64(0)

	var nodeAddresses = make(map[NodeID][]nodeAddrConnectability)
	nodeAddresseFlat := []*nodeAddrConnectability{}

	if err := a.cfg.Graph.ForEachNode(func(n Node) error {
		if n.PubKey().IsEqual(a.cfg.Self) {
			log.Warnf("connecting to ourself would be silly.")
			return nil
		}
		//we will only be here for nodes with addresses.
		nodesWithAddr++
		nID := NewNodeID(n.PubKey())
		nodeAddresses[nID] = []nodeAddrConnectability{}
		for _, addr := range n.Addrs() {
			totalAddress++
			connectability := nodeAddrConnectability{nID: nID, node: n, addr: addr, count: totalAddress}
			nodeAddresses[nID] = append(nodeAddresses[nID], connectability)
			nodeAddresseFlat = append(nodeAddresseFlat, &connectability)
			// log.Warnf("some node with addr %s", addr)
		}
		return nil
	}); err != nil {
		return
	}

	connectorChan := make(chan *nodeAddrConnectability, totalAddress)
	doneChan := make(chan *nodeAddrConnectability, totalAddress)
	go func() {
		diong := 0
		for _, addr := range nodeAddresseFlat {
			connectorChan <- addr
			//sleep to not hammer.
			diong++
			// log.Warnf("added %s to connectorChan (%d/%d)", addr.addr, diong, totalAddress)
			// time.Sleep(100 * time.Millisecond)
		}
		close(connectorChan)
	}()
	//another goroutine to execute that

	var junkIP = regexp.MustCompile(`^127\.0\.0\.1:`)

	expectResults := int64(0)
	bailAfterExpectResults := int64(0)
	go func() {
		wg := sync.WaitGroup{}

		connectAtOnce := 25 //TODO make this config'able.
		wg.Add(connectAtOnce)
		workAccepted := int64(0)
		server := a.cfg.PeerScannerServer

		for k := 0; k < connectAtOnce; k++ {
			go func() {
				defer wg.Done()
				for addr := range connectorChan {
					atomic.AddInt64(&workAccepted, 1)
					if (bailAfterExpectResults > 0) && (expectResults >= bailAfterExpectResults) {
						log.Warnf("bailing after finding %d that we can talk to (DEBUG)", expectResults)
						return
					}

					// // First, we'll check if we're already connected to the target peer. If
					// // not, then we'll need to establish a connection.
					nodePubKey := addr.node.PubKey()
					addr.startedAt = time.Now().UTC()

					if server.ConnectedToNode(nodePubKey) {
						log.Warnf("already connected to the node (%x) that owns %s (cool!)", addr.nID, addr.addr)
						continue
					}
					lnAddr, err := server.GetLnAddr(nodePubKey, addr.addr)
					if err != nil {
						log.Warnf("getLnAddr Error: %s", err.Error())
						continue
					}
					if junkIP.MatchString(addr.addr.String()) {
						log.Warnf("not going to try to connect to %s (lol)", addr.addr)
						continue
					}
					log.Warnf("can we connect to? %s %d / %d (addr.count %d)", addr.addr, workAccepted, totalAddress, addr.count)

					addr.started = true

					////////////// DEBUG ///////////////////
					// time.Sleep(time.Duration(50+rand.Int63n(300)) * time.Millisecond)
					// if rand.Intn(5) == 0 {
					// 	addr.succeded = true
					// 	atomic.AddInt64(&expectResults, 1)
					// 	doneChan <- addr
					// }
					// continue
					////////////// DEBUG ///////////////////

					// TODO(roasbeef): make perm connection in server after
					// chan open?
					err = server.ConnectToPeer(lnAddr, false)
					addr.finished = true
					if err != nil {
						// If we weren't able to connect to the peer,
						// then we'll move onto the next.
						log.Warnf("connect err? %s ->  %s ", lnAddr.Address, err.Error())
						continue
					}

					addr.succeded = true
					//TODO: figure out how to deal with this error that we see from some peers when trying to open channels later ..
					//    "rpc error: code = Code(208) desc = local/remote feerates are too different"
					//    seems to be a c-lightning thing. (see https://github.com/ElementsProject/lightning/issues/443)
					//    ideally i'd like to be able to determine that the policies are mismatched right here so that we know not to bother with this peer.
					log.Warnf("WOOTWOOT actually connected to %x@%s (result #%d)", addr.nID, lnAddr.Address, expectResults+1)

					err = server.DisconnectPeer(nodePubKey)
					if err != nil {
						log.Warnf("disconnect err? %s -> %s ", lnAddr.Address, err.Error())
					}

					atomic.AddInt64(&expectResults, 1)
					doneChan <- addr
				}
			}()
		}
		log.Warnf("waiting for %d simultaneous goroutines to plow through %d connection attempts.", connectAtOnce, totalAddress)
		wg.Wait()
		close(doneChan)
		log.Warnf("done waiting, closed doneChan")
	}()

	stopReporter := false
	go func() {
		for {
			// started := 0
			finished := 0
			activeCount := 0
			for _, v := range nodeAddresseFlat {
				if v.finished {
					finished++
				}
				if v.started && !v.finished {
					activeCount += 1
					runningFor := time.Since(v.startedAt) / time.Millisecond
					log.Warnf("REPORTER connection task %d/%d running for %d msec (%s)", v.count, totalAddress, runningFor, v.addr)
				}
			}
			log.Warnf("REPORTER believes %d connection attempts are currently actively being waited on, finsiehd %d/%d", activeCount, finished, totalAddress)
			time.Sleep(10 * time.Second)
			if stopReporter {
				break
			}
		}
	}()

	resultCount := int64(0)
	connectable := 0
	log.Warnf("result of checking those things? ...")
	for addr := range doneChan {
		//do something with results.
		resultCount++
		log.Warnf("got a result #. %d (%d/%d)", addr.count, resultCount, expectResults)
		if addr.succeded {
			connectable++
		}
	}
	stopReporter = true

	//being here means we've collected all the results.
	if resultCount != expectResults {
		panic("go learn how channels work clearly?")
	}

	log.Warnf("we could connect to %d out of %d addresses we tried.", connectable, totalAddress)
	log.Warnf("that was a total %d nodes with addresses, total of addresses %d", nodesWithAddr, totalAddress)

	connectableNodeAddresses := make([]nodeAddrConnectability, connectable)
	unconnectableNodes := map[NodeID]struct{}{}
	connectableIndex := 0
	for _, v := range nodeAddresseFlat {
		if !v.succeded {
			unconnectableNodes[v.nID] = struct{}{}
			continue
		}
		connectableNodeAddresses[connectableIndex] = *v
		connectableIndex++
		delete(unconnectableNodes, v.nID) //just in case we added the node to unconnectables because one of its other addresses could not connect to.
	}

	// log.Warnf("now go to sleep little baby.")
	// time.Sleep(10 * time.Second)

	a.connectableNodeAddressesLock.Lock()
	a.connectableNodeAddresses = &connectableNodeAddresses
	a.unconnectableNodes = &unconnectableNodes
	a.connectableNodeAddressesLock.Unlock()

	//if we were doing this peer scanning then our autopilot wakeups should come from here
	a.WakeUp()
	return
	// return connectableNodeAddresses, nil
}

func (a *Agent) getConnectableNodesList() *[]nodeAddrConnectability {
	a.connectableNodeAddressesLock.Lock()
	defer a.connectableNodeAddressesLock.Unlock()
	return a.connectableNodeAddresses
}
func (a *Agent) getUnconnectableNodesMap() *map[NodeID]struct{} {
	a.connectableNodeAddressesLock.Lock()
	defer a.connectableNodeAddressesLock.Unlock()
	return a.unconnectableNodes
}

func (a *Agent) startPeerScanner() {
	if !a.cfg.PeerScanner {
		return
	}

	go func() {
		a.checkNodeConnectivity()
		log.Warnf("peerscanner sleeping for a while before running again.")
		time.Sleep(30 * time.Minute)
	}()
}
