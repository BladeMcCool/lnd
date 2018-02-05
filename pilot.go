package main

import (
	"errors"
	"fmt"
	"net"
	_ "sync"
	_ "sync/atomic"
	_ "time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// chanController is an implementation of the autopilot.ChannelController
// interface that's backed by a running lnd instance.
type chanController struct {
	server *server
	// connectableNodeAddressesLock sync.Mutex
	// connectableNodeAddresses     *[]nodeAddrConnectability
}

type peerscannerServer struct {
	server *server
}

// OpenChannel opens a channel to a target peer, with a capacity of the
// specified amount. This function should un-block immediately after the
// funding transaction that marks the channel open has been broadcast.
func (c *chanController) OpenChannel(target *btcec.PublicKey,
	amt btcutil.Amount, addrs []net.Addr) error {

	// We can't establish a channel if no addresses were provided for the
	// peer.

	if len(addrs) == 0 {
		return fmt.Errorf("Unable to create channel w/o an active " +
			"address")
	}

	// First, we'll check if we're already connected to the target peer. If
	// not, then we'll need to establish a connection.
	if _, err := c.server.FindPeer(target); err != nil {
		atplLog.Tracef("Connecting to %x to auto-create channel: ",
			target.SerializeCompressed())

		lnAddr := &lnwire.NetAddress{
			IdentityKey: target,
			ChainNet:    activeNetParams.Net,
		}

		// We'll attempt to successively connect to each of the
		// advertised IP addresses until we've either exhausted the
		// advertised IP addresses, or have made a connection.
		var connected bool

		// We will try each address and go with the first one that connects us.
		for _, addr := range addrs {
			// If the address doesn't already have a port, then
			// we'll assume the current default port.
			tcpAddr, ok := addr.(*net.TCPAddr)
			if !ok {
				return fmt.Errorf("TCP address required instead "+
					"have %T", addr)
			}
			if tcpAddr.Port == 0 {
				tcpAddr.Port = defaultPeerPort
			}

			lnAddr.Address = tcpAddr

			// TODO(roasbeef): make perm connection in server after
			// chan open?
			err := c.server.ConnectToPeer(lnAddr, false)
			if err != nil {
				atplLog.Warnf("OpenChannel ConnectToPeer %s error %s", tcpAddr, err.Error())

				// If we weren't able to connect to the peer,
				// then we'll move onto the next.
				continue
			}

			connected = true
			break
		}

		// If we weren't able to establish a connection at all, then
		// we'll error out.
		if !connected {
			return fmt.Errorf("Unable to connect to %x",
				target.SerializeCompressed())
		}
	}

	// With the connection established, we'll now establish our connection
	// to the target peer, waiting for the first update before we exit.
	feePerWeight, err := c.server.cc.feeEstimator.EstimateFeePerWeight(3)
	if err != nil {
		return err
	}

	// TODO(halseth): make configurable?
	minHtlc := lnwire.NewMSatFromSatoshis(1)

	updateStream, errChan := c.server.OpenChannel(-1, target, amt, 0,
		minHtlc, feePerWeight, false)

	select {
	case err := <-errChan:
		return err
	case <-updateStream:
		return nil
	case <-c.server.quit:
		return nil
	}
}

func (c *chanController) CloseChannel(chanPoint *wire.OutPoint) error {
	return nil
}
func (c *chanController) SpliceIn(chanPoint *wire.OutPoint,
	amt btcutil.Amount) (*autopilot.Channel, error) {
	return nil, nil
}
func (c *chanController) SpliceOut(chanPoint *wire.OutPoint,
	amt btcutil.Amount) (*autopilot.Channel, error) {
	return nil, nil
}

func (s *peerscannerServer) ConnectedToNode(key *btcec.PublicKey) bool {
	_, err := s.server.FindPeer(key)
	return (err == nil)
}
func (s *peerscannerServer) ConnectToPeer(addr *lnwire.NetAddress, perm bool) error {
	return s.server.ConnectToPeer(addr, perm)
}
func (s *peerscannerServer) DisconnectPeer(pubKey *btcec.PublicKey) error {
	return s.server.DisconnectPeer(pubKey)
}

func (s *peerscannerServer) GetLnAddr(nodePubKey *btcec.PublicKey, addr net.Addr) (*lnwire.NetAddress, error) {
	//TODO re-use this code in the place i got this from somehow??
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return nil, errors.New(fmt.Sprintf("TCP address required instead have %T", addr))
	}
	if tcpAddr.Port == 0 {
		tcpAddr.Port = defaultPeerPort
	}

	lnAddr := &lnwire.NetAddress{
		IdentityKey: nodePubKey,
		ChainNet:    activeNetParams.Net,
		Address:     tcpAddr,
	}
	return lnAddr, nil
}

func (s *peerscannerServer) UnspentWitnessOutputCount() (int, error) {
	coins, err := s.server.cc.wallet.ListUnspentWitness(1)
	if err != nil {
		return 0, err
	}
	return len(coins), nil
}

// func (c *chanController) checkNodeConnectivity(self *btcec.PublicKey, graph autopilot.ChannelGraph) {
// 	//what are the nodes?
// 	_, _ = self, graph
// 	nodesWithAddr := 0
// 	totalAddress := int64(0)

// 	var nodeAddresses = make(map[autopilot.NodeID][].NodeAddrConnectability)
// 	nodeAddresseFlat := []*nodeAddrConnectability{}

// 	if err := graph.ForEachNode(func(n autopilot.Node) error {
// 		if n.PubKey().IsEqual(self) {
// 			atplLog.Warnf("connecting to ourself would be silly.")
// 			return nil
// 		}
// 		//we will only be here for nodes with addresses.
// 		nodesWithAddr++
// 		nID := autopilot.NewNodeID(n.PubKey())
// 		nodeAddresses[nID] = []nodeAddrConnectability{}
// 		for _, addr := range n.Addrs() {
// 			totalAddress++
// 			connectability := nodeAddrConnectability{nID: nID, node: n, addr: addr, count: totalAddress}
// 			nodeAddresses[nID] = append(nodeAddresses[nID], connectability)
// 			nodeAddresseFlat = append(nodeAddresseFlat, &connectability)
// 			// atplLog.Warnf("some node with addr %s", addr)
// 		}
// 		return nil
// 	}); err != nil {
// 		return errors.New("woops")
// 	}

// 	connectorChan := make(chan *nodeAddrConnectability, totalAddress)
// 	doneChan := make(chan *nodeAddrConnectability, totalAddress)
// 	go func() {
// 		diong := 0
// 		for _, addr := range nodeAddresseFlat {
// 			connectorChan <- addr
// 			//sleep to not hammer.
// 			diong++
// 			atplLog.Warnf("added %s to connectorChan (%d/%d)", addr.addr, diong, totalAddress)
// 			// time.Sleep(100 * time.Millisecond)
// 		}
// 		close(connectorChan)
// 	}()
// 	//another goroutine to execute that

// 	expectResults := int64(0)
// 	go func() {
// 		wg := sync.WaitGroup{}

// 		connectAtOnce := 25
// 		wg.Add(connectAtOnce)
// 		workAccepted := int64(0)
// 		for k := 0; k < connectAtOnce; k++ {
// 			go func() {
// 				defer wg.Done()
// 				for addr := range connectorChan {
// 					atomic.AddInt64(&workAccepted, 1)
// 					// // First, we'll check if we're already connected to the target peer. If
// 					// // not, then we'll need to establish a connection.
// 					nodePubKey := addr.node.PubKey()
// 					addr.startedAt = time.Now().UTC()
// 					if _, err := c.server.FindPeer(nodePubKey); err == nil {
// 						atplLog.Warnf("already connected to %s (cool!)", addr.addr)
// 						continue
// 					}

// 					atplLog.Warnf("can we connect to? %s %d / %d (addr.count %d)", addr.addr, workAccepted, totalAddress, addr.count)
// 					tcpAddr, ok := addr.addr.(*net.TCPAddr)
// 					if !ok {
// 						atplLog.Errorf("TCP address required instead have %T", addr)
// 						continue
// 					}
// 					if tcpAddr.Port == 0 {
// 						tcpAddr.Port = defaultPeerPort
// 					}

// 					lnAddr := &lnwire.NetAddress{
// 						IdentityKey: nodePubKey,
// 						ChainNet:    activeNetParams.Net,
// 						Address:     tcpAddr,
// 					}

// 					addr.started = true

// 					////////////// DEBUG ///////////////////
// 					// time.Sleep(time.Duration(50+rand.Int63n(300)) * time.Millisecond)
// 					// if rand.Intn(5) == 0 {
// 					// 	addr.succeded = true
// 					// 	atomic.AddInt64(&expectResults, 1)
// 					// 	doneChan <- addr
// 					// }
// 					// continue
// 					////////////// DEBUG ///////////////////

// 					// TODO(roasbeef): make perm connection in server after
// 					// chan open?
// 					err := c.server.ConnectToPeer(lnAddr, false)
// 					addr.finished = true
// 					if err != nil {
// 						// If we weren't able to connect to the peer,
// 						// then we'll move onto the next.
// 						atplLog.Warnf("connect err? %s ->  %s ", tcpAddr, err.Error())
// 						continue
// 					}
// 					addr.succeded = true
// 					atplLog.Warnf("WOOTWOOT actually connected to %s", tcpAddr)

// 					err = c.server.DisconnectPeer(nodePubKey)
// 					if err != nil {
// 						atplLog.Warnf("disconnect err? %s -> %s ", tcpAddr, err.Error())
// 					}

// 					atomic.AddInt64(&expectResults, 1)
// 					doneChan <- addr
// 				}
// 			}()
// 		}
// 		atplLog.Warnf("waiting for %d simultaneous goroutines to plow through %d connection attempts.", connectAtOnce, totalAddress)
// 		wg.Wait()
// 		close(doneChan)
// 		atplLog.Warnf("done waiting, closed doneChan")
// 	}()

// 	stopReporter := false
// 	go func() {
// 		for {
// 			// started := 0
// 			finished := 0
// 			activeCount := 0
// 			for _, v := range nodeAddresseFlat {
// 				if v.finished {
// 					finished++
// 				}
// 				if v.started && !v.finished {
// 					activeCount += 1
// 					runningFor := time.Since(v.startedAt) / time.Millisecond
// 					atplLog.Warnf("REPORTER connection task %d running for %d msec (%s)", v.count, runningFor, v.addr)
// 				}
// 			}
// 			atplLog.Warnf("REPORTER believes %d connection attempts are currently actively being waited on, finsiehd %d/%d", activeCount, finished, totalAddress)
// 			time.Sleep(10 * time.Second)
// 			if stopReporter {
// 				break
// 			}
// 		}
// 	}()

// 	resultCount := int64(0)
// 	connectable := 0
// 	atplLog.Warnf("result of checking those things? ...")
// 	for addr := range doneChan {
// 		//do something with results.
// 		resultCount++
// 		atplLog.Warnf("got a result #. %d (%d/%d)", addr.count, resultCount, expectResults)
// 		if addr.succeded {
// 			connectable++
// 		}
// 	}
// 	stopReporter = true

// 	//being here means we've collected all the results.
// 	if resultCount != expectResults {
// 		panic("go learn how channels work clearly?")
// 	}

// 	atplLog.Warnf("we could connect to %d out of %d addresses we tried.", connectable, totalAddress)
// 	atplLog.Warnf("that was a total %d nodes with addresses, total of addresses %d", nodesWithAddr, totalAddress)

// 	connectableNodeAddresses := make([]*nodeAddrConnectability, connectable)
// 	for i, v := range nodeAddresseFlat {
// 		connectableNodeAddresses[i] = v
// 	}

// 	// atplLog.Warnf("now go to sleep little baby.")
// 	// time.Sleep(10 * time.Second)

// 	c.connectableNodeAddressesLock.Lock()
// 	c.connectableNodeAddresses = &connectableNodeAddresses
// 	c.connectableNodeAddressesLock.Unlock()

// 	return
// 	// return connectableNodeAddresses, nil
// }

// func (c *chanController) startPeerScanner(self *btcec.PublicKey, graph autopilot.ChannelGraph) {
// 	go func() {
// 		c.GetConnectableNodesList(self, graph)
// 		time.Sleep(30 * time.Minute())
// 	}()
// }

// func (c *chanController) GetConnectableNodesList() []autopilot.error {
// 	c.connectableNodeAddressesLock.Lock()
// 	defer c.connectableNodeAddressesLock.Unlock()
// 	return c.connectableNodeAddresses
// }

// A compile time assertion to ensure chanController meets the
// autopilot.ChannelController interface.
var _ autopilot.ChannelController = (*chanController)(nil)

// initAutoPilot initializes a new autopilot.Agent instance based on the passed
// configuration struct. All interfaces needed to drive the pilot will be
// registered and launched.
func initAutoPilot(svr *server, cfg *autoPilotConfig) (*autopilot.Agent, error) {
	atplLog.Infof("Instantiating autopilot with cfg: %v", spew.Sdump(cfg))

	// First, we'll create the preferential attachment heuristic,
	// initialized with the passed auto pilot configuration parameters.
	//
	minChanSize := svr.cc.wallet.Cfg.DefaultConstraints.DustLimit * 5
	attachmentMaxFundingAmount := maxFundingAmount
	cfgMaxFundAmt := btcutil.Amount(cfg.MaxFundingAmount)
	if (cfgMaxFundAmt > 0) && (cfgMaxFundAmt < maxFundingAmount) {
		attachmentMaxFundingAmount = cfgMaxFundAmt
	}

	// TODO(roasbeef): switch here to dispatch specified heuristic
	var attachmentHeuristic autopilot.AttachmentHeuristic
	switch cfg.Heuristic {
	default:
		atplLog.Infof("will use default (prefattach) heuristic.")
		attachmentHeuristic = autopilot.NewConstrainedPrefAttachment(minChanSize, attachmentMaxFundingAmount, uint16(cfg.MaxChannels), cfg.Allocation)
	case "experimental":
		atplLog.Infof("will use experimental attachment heuristic.")
		attachmentHeuristic = autopilot.NewHackyPrefAttachment(minChanSize, attachmentMaxFundingAmount, uint16(cfg.MaxChannels), cfg.Allocation)
	}

	// With the heuristic itself created, we can now populate the remainder
	// of the items that the autopilot agent needs to perform its duties.
	self := svr.identityPriv.PubKey()
	pilotCfg := autopilot.Config{
		Self:              self,
		Heuristic:         attachmentHeuristic,
		ChanController:    &chanController{svr},
		PeerScanner:       cfg.PeerScanner,
		PeerScannerServer: &peerscannerServer{svr},
		WalletBalance: func() (btcutil.Amount, error) {
			return svr.cc.wallet.ConfirmedBalance(1, true)
		},
		Graph: autopilot.ChannelGraphFromDatabase(svr.chanDB.ChannelGraph()),
	}

	// Next, we'll fetch the current state of open channels from the
	// database to use as initial state for the auto-pilot agent.
	activeChannels, err := svr.chanDB.FetchAllChannels()
	if err != nil {
		return nil, err
	}
	initialChanState := make([]autopilot.Channel, len(activeChannels))
	for i, channel := range activeChannels {
		initialChanState[i] = autopilot.Channel{
			ChanID:      channel.ShortChanID,
			Capacity:    channel.Capacity,
			IsInitiator: channel.IsInitiator,
			Node:        autopilot.NewNodeID(channel.IdentityPub),
		}
	}

	// Now that we have all the initial dependencies, we can create the
	// auto-pilot instance itself.
	pilot, err := autopilot.New(pilotCfg, initialChanState)
	if err != nil {
		return nil, err
	}

	// Finally, we'll need to subscribe to two things: incoming
	// transactions that modify the wallet's balance, and also any graph
	// topology updates.
	txnSubscription, err := svr.cc.wallet.SubscribeTransactions()
	if err != nil {
		return nil, err
	}
	graphSubscription, err := svr.chanRouter.SubscribeTopology()
	if err != nil {
		return nil, err
	}

	// We'll launch a goroutine to provide the agent with notifications
	// whenever the balance of the wallet changes.
	svr.wg.Add(2)
	go func() {
		defer txnSubscription.Cancel()
		defer svr.wg.Done()

		for {
			select {
			case txnUpdate := <-txnSubscription.ConfirmedTransactions():
				pilot.OnBalanceChange(txnUpdate.Value)
			case <-svr.quit:
				return
			}
		}

	}()
	go func() {
		defer svr.wg.Done()

		for {
			select {
			// We won't act upon new unconfirmed transaction, as
			// we'll only use confirmed outputs when funding.
			// However, we will still drain this request in order
			// to avoid goroutine leaks, and ensure we promptly
			// read from the channel if available.
			case <-txnSubscription.UnconfirmedTransactions():
			case <-svr.quit:
				return
			}
		}

	}()

	// We'll also launch a goroutine to provide the agent with
	// notifications for when the graph topology controlled by the node
	// changes.
	svr.wg.Add(1)
	go func() {
		defer graphSubscription.Cancel()
		defer svr.wg.Done()

		for {
			select {
			case topChange, ok := <-graphSubscription.TopologyChanges:
				// If the router is shutting down, then we will
				// as well.
				if !ok {
					return
				}

				for _, edgeUpdate := range topChange.ChannelEdgeUpdates {
					// If this isn't an advertisement by
					// the backing lnd node, then we'll
					// continue as we only want to add
					// channels that we've created
					// ourselves.
					if !edgeUpdate.AdvertisingNode.IsEqual(self) {
						continue
					}

					// If this is indeed a channel we
					// opened, then we'll convert it to the
					// autopilot.Channel format, and notify
					// the pilot of the new channel.
					chanNode := autopilot.NewNodeID(
						edgeUpdate.ConnectingNode,
					)
					chanID := lnwire.NewShortChanIDFromInt(
						edgeUpdate.ChanID,
					)
					edge := autopilot.Channel{
						ChanID:   chanID,
						Capacity: edgeUpdate.Capacity,
						Node:     chanNode,
					}
					pilot.OnChannelOpen(edge)
				}

				// For each closed closed channel, we'll obtain
				// the chanID of the closed channel and send it
				// to the pilot.
				for _, chanClose := range topChange.ClosedChannels {
					chanID := lnwire.NewShortChanIDFromInt(
						chanClose.ChanID,
					)

					pilot.OnChannelClose(chanID)
				}

			case <-svr.quit:
				return
			}
		}
	}()

	return pilot, nil
}
