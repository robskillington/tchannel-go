package tchannel

import (
	"fmt"
	"math/rand"
	"sync"
)

type relayItem struct {
	remapID     uint32
	destination *Relay
}

// ServiceHosts keeps track of the hosts registered to a service.
type ServiceHosts struct {
	sync.RWMutex

	r        *rand.Rand
	randLock sync.Mutex
	peers    map[string][]string
}

// NewServiceHosts creates a new empty ServiceHosts.
func NewServiceHosts() *ServiceHosts {
	return &ServiceHosts{
		r:     rand.New(rand.NewSource(rand.Int63())),
		peers: make(map[string][]string),
	}
}

// Register registers a peer for the given service.
func (h *ServiceHosts) Register(service, hostPort string) {
	h.Lock()
	h.peers[service] = append(h.peers[service], hostPort)
	h.Unlock()
}

// GetHostPort returns a random host:port to use for the given service
func (h *ServiceHosts) GetHostPort(service string) string {
	h.RLock()
	hostPorts := h.peers[service]
	h.RUnlock()
	if len(hostPorts) == 0 {
		return ""
	}

	h.randLock.Lock()
	randHost := h.r.Intn(len(hostPorts))
	h.randLock.Unlock()

	return hostPorts[randHost]
}

// Relay contains all relay specific information.
type Relay struct {
	sync.RWMutex
	connections   map[uint32]relayItem
	serviceHosts  *ServiceHosts
	statsReporter StatsReporter

	// Immutable
	ch   *Channel
	conn *Connection
}

// NewRelay creates a relay.
func NewRelay(ch *Channel, conn *Connection) *Relay {
	return &Relay{
		ch:            ch,
		serviceHosts:  ch.serviceHosts,
		statsReporter: conn.statsReporter,
		conn:          conn,
		connections:   make(map[uint32]relayItem),
	}
}

// Receive receives a frame from another relay, and sends it to the underlying connection.
func (r *Relay) Receive(frame *Frame) {
	//r.conn.log.Debugf("Relay received frame %v", frame.Header)
	r.conn.sendCh <- frame
}

// addRelay adds a relay that will remap IDs from id to remapID
// and then send the frame to the given destination relay.
func (r *Relay) addRelay(id, remapID uint32, destination *Relay) relayItem {
	newRelay := relayItem{
		remapID:     remapID,
		destination: destination,
	}

	r.Lock()
	r.connections[id] = newRelay
	r.Unlock()
	return newRelay
}

// RelayFrame relays the given frame.
// TODO(prashant): Remove the id from the map once that sequence is complete.
func (r *Relay) RelayFrame(frame *Frame) {
	if frame.MessageType() != messageTypeCallReq {
		r.RLock()
		relay, ok := r.connections[frame.Header.ID]
		r.RUnlock()
		if !ok {
			panic(fmt.Sprintf("got non-call req for inactive ID: %v", frame.Header.ID))
		}
		frame.Header.ID = relay.remapID
		relay.destination.Receive(frame)
		return
	}

	if _, ok := r.connections[frame.Header.ID]; ok {
		panic(fmt.Sprintf("callReq with already active ID: %b", frame.Header.ID))
	}

	// Get the destination
	svc := string(frame.Service())
	hostPort := r.serviceHosts.GetHostPort(svc)
	peer := r.ch.Peers().GetOrAdd(hostPort)

	c, err := peer.GetConnectionForRelay()
	if err != nil {
		r.ch.Logger().Warnf("failed to connect to %v: %v", hostPort, err)
		// TODO : return an error frame.
		return
	}

	destinationID := c.NextMessageID()
	c.relay.addRelay(destinationID, frame.Header.ID, r)
	r.statsReporter.IncCounter("relay", nil, 1)
	relayToDest := r.addRelay(frame.Header.ID, destinationID, c.relay)

	frame.Header.ID = destinationID
	relayToDest.destination.Receive(frame)
}