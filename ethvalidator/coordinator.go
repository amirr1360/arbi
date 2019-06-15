/*
 * Copyright 2019, Offchain Labs, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package ethvalidator

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/golang/protobuf/proto"
	"github.com/gorilla/websocket"
	"github.com/offchainlabs/arb-avm/value"
	"github.com/offchainlabs/arb-validator/valmessage"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/offchainlabs/arb-avm/protocol"
	"github.com/offchainlabs/arb-avm/vm"
)

type Client struct {
	cm         *ClientManager
	ToClient   chan *ValidatorRequest
	FromClient chan *FollowerResponse

	conn    *websocket.Conn
	Address common.Address
}

func NewClient(cm *ClientManager, conn *websocket.Conn, address common.Address) *Client {
	return &Client{
		cm,
		make(chan *ValidatorRequest, 128),
		make(chan *FollowerResponse, 128),
		conn,
		address,
	}
}

func (c *Client) readPump() {
	defer func() {
		c.cm.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error { c.conn.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("error: %v", err)
			}
			break
		}

		r := &FollowerResponse{}
		err = proto.Unmarshal(message, r)
		if err != nil {
			log.Println("Recieved bad message from follower")
			continue
		}
		c.FromClient <- r
	}
}

// writePump pumps messages from the hub to the websocket connection.
//
// A goroutine running writePump is started for each connection. The
// application ensures that there is at most one writer to a connection by
// executing all writes from this goroutine.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.ToClient:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// The hub closed the channel.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			raw, err := proto.Marshal(message)
			if err != nil {
				log.Fatalln("Follower failed to marshal response")
			}
			w.Write(raw)
			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

type ValidatorLeaderRequest interface {
}

//type ValidatorMessageRequest interface {
//	msg vm.
//}

type LabeledFollowerResponse struct {
	address  common.Address
	response *FollowerResponse
}

type ClientManager struct {
	clients         map[*Client]bool
	broadcast       chan *ValidatorRequest
	register        chan *Client
	unregister      chan *Client
	waitRequestChan chan chan bool
	sigRequestChan  chan GatherSignatureRequest
	waitingChans    map[chan bool]bool
	responses       map[[32]byte]chan LabeledFollowerResponse

	key        *ecdsa.PrivateKey
	vmId       [32]byte
	validators map[common.Address]validatorInfo
}

func NewClientManager(key *ecdsa.PrivateKey, vmId [32]byte, validators map[common.Address]validatorInfo) *ClientManager {
	return &ClientManager{
		clients:         make(map[*Client]bool),
		broadcast:       make(chan *ValidatorRequest, 10),
		register:        make(chan *Client, 10),
		unregister:      make(chan *Client, 10),
		waitRequestChan: make(chan chan bool, 128),
		sigRequestChan:  make(chan GatherSignatureRequest, 10),
		waitingChans:    make(map[chan bool]bool),
		responses:       make(map[[32]byte]chan LabeledFollowerResponse),
		key:             key,
		vmId:            vmId,
		validators:      validators,
	}
}

type GatherSignatureRequest struct {
	request      *ValidatorRequest
	responseChan chan LabeledFollowerResponse
	requestID    [32]byte
}

func (m *ClientManager) Run() {
	aggResponseChan := make(chan LabeledFollowerResponse, 32)
	for {
		select {
		case waitRequest := <-m.waitRequestChan:
			if len(m.clients) == len(m.validators)-1 {
				waitRequest <- true
			} else {
				m.waitingChans[waitRequest] = true
			}
		case response := <-aggResponseChan:
			m.responses[value.NewHashFromBuf(response.response.RequestId)] <- response
		case request := <-m.sigRequestChan:
			m.broadcast <- request.request
			m.responses[request.requestID] = request.responseChan
		case client := <-m.register:
			m.clients[client] = true
			go func() {
				for response := range client.FromClient {
					aggResponseChan <- LabeledFollowerResponse{client.Address, response}
				}
			}()
			if len(m.clients) == len(m.validators)-1 {
				for waitChan := range m.waitingChans {
					waitChan <- true
				}
				m.waitingChans = make(map[chan bool]bool)
			}
		case client := <-m.unregister:
			if _, ok := m.clients[client]; ok {
				delete(m.clients, client)
				close(client.ToClient)
			}
		case message := <-m.broadcast:
			for client := range m.clients {
				select {
				case client.ToClient <- message:
				default:
					close(client.ToClient)
					delete(m.clients, client)
				}
			}
		}
	}
}

func (m *ClientManager) gatherSignatures(
	request *ValidatorRequest,
	requestID [32]byte,
) []LabeledFollowerResponse {
	responseChan := make(chan LabeledFollowerResponse, len(m.validators)-1)
	log.Println("Coordinator gathering signatures")
	m.sigRequestChan <- GatherSignatureRequest{
		request,
		responseChan,
		requestID,
	}
	responseList := make([]LabeledFollowerResponse, 0, len(m.validators)-1)
	timer := time.NewTimer(20 * time.Second)
	timedOut := false
	defer timer.Stop()
	for {
		select {
		case response := <-responseChan:
			responseList = append(responseList, response)
		case <-timer.C:
			log.Println("Coordinator timed out gathering signatures")
			timedOut = true
		}
		if len(responseList) == len(m.validators)-1 || timedOut {
			break
		}
	}
	return responseList
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func (m *ClientManager) RunServer() error {
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println(err)
			return
		}
		tlsCon, ok := conn.UnderlyingConn().(*tls.Conn)
		if !ok {
			log.Println("Made non tls connection")
			return
		}

		_, signedUnique, err := conn.ReadMessage()
		uniqueVal := tlsCon.ConnectionState().TLSUnique
		hashVal := crypto.Keccak256(uniqueVal)
		pubkey, err := crypto.SigToPub(hashVal, signedUnique)
		if err != nil {
			log.Println(err)
			return
		}
		address := crypto.PubkeyToAddress(*pubkey)
		if _, ok := m.validators[address]; !ok {
			log.Println("Follower tried to connect with bad pubkey")
			return
		}
		sigData, err := crypto.Sign(hashVal, m.key)
		wr, err := conn.NextWriter(websocket.BinaryMessage)
		wr.Write(m.vmId[:])
		wr.Write(sigData)

		if err := wr.Close(); err != nil {
			log.Println(err)
			return
		}
		c := NewClient(m, conn, address)
		log.Println("Coordinator connected with follower", hexutil.Encode(address[:]))
		m.register <- c

		go c.readPump()
		go c.writePump()
	})
	return http.ListenAndServeTLS(":1236", "server.crt", "server.key", nil)
}

func (m *ClientManager) WaitForFollowers(timeout time.Duration) bool {
	waitChan := make(chan bool, 1)
	m.waitRequestChan <- waitChan
	select {
	case <-waitChan:
		return true
	case <-time.After(timeout):
		return false
	}
}

type OffchainMessage struct {
	Message   protocol.Message
	Signature []byte
}

func (m *MessageProcessingQueue) Fetch() chan []OffchainMessage {
	retChan := make(chan []OffchainMessage, 1)
	m.requests <- retChan
	return retChan
}

func (m *MessageProcessingQueue) HasMessages() chan bool {
	retChan := make(chan bool, 1)
	m.requests <- retChan
	return retChan
}

func (m *MessageProcessingQueue) Return(messages []OffchainMessage) {
	m.requests <- messages
}

func (m *MessageProcessingQueue) Send(message OffchainMessage) {
	m.requests <- message
}

type MessageProcessingQueue struct {
	queuedMessages []OffchainMessage
	requests       chan interface{}
}

func NewMessageProcessingQueue() *MessageProcessingQueue {
	return &MessageProcessingQueue{
		queuedMessages: make([]OffchainMessage, 0),
		requests:       make(chan interface{}, 10),
	}
}

func (m *MessageProcessingQueue) run() {
	go func() {
		for {
			request := <-m.requests
			switch request := request.(type) {
			case chan []OffchainMessage:
				request <- m.queuedMessages
				m.queuedMessages = nil
			case []OffchainMessage:
				m.queuedMessages = append(request, m.queuedMessages...)
			case OffchainMessage:
				m.queuedMessages = append(m.queuedMessages, request)
			case chan bool:
				request <- len(m.queuedMessages) > 0
			default:
				log.Fatalf("Unhandled request type %T\n", request)
			}
		}
	}()
}

type ValidatorCoordinator struct {
	Val *EthValidator
	cm  *ClientManager

	requestChan chan ValidatorLeaderRequest

	mpq *MessageProcessingQueue
}

func NewValidatorCoordinator(
	name string,
	machine *vm.Machine,
	key *ecdsa.PrivateKey,
	config *valmessage.VMConfiguration,
	challengeEverything bool,
	connectionInfo ArbAddresses,
	ethURL string,
) (*ValidatorCoordinator, error) {
	var vmId [32]byte
	_, err := rand.Read(vmId[:])
	if err != nil {
		log.Fatal(err)
	}

	c, err := NewEthValidator(name, vmId, machine, key, config, challengeEverything, connectionInfo, ethURL)
	if err != nil {
		return nil, err
	}
	return &ValidatorCoordinator{
		Val:         c,
		cm:          NewClientManager(key, vmId, c.Validators),
		requestChan: make(chan ValidatorLeaderRequest, 10),
		mpq:         NewMessageProcessingQueue(),
	}, nil
}

func (m *ValidatorCoordinator) SendMessage(msg OffchainMessage) {
	m.mpq.Send(msg)
}

func (m *ValidatorCoordinator) Run() {
	go func() {
		err := m.cm.RunServer()
		fmt.Println("Running server", err)
		if err != nil {
			log.Fatal(err)
		}
	}()
	go m.mpq.run()
	go m.cm.Run()
	m.Val.StartListening()
	go func() {
		pendingForProcessing := false
		for {
			select {
			case request := <-m.requestChan:
				switch request := request.(type) {
				case CoordinatorCreateRequest:
					ret, err := m.createVMImpl(request.timeout)
					if err != nil {
						request.errChan <- err
					} else {
						request.retChan <- ret
					}
				case CoordinatorDisputableRequest:
					request.retChan <- m.initiateDisputableAssertionImpl()
				case CoordinatorUnanimousRequest:
					ret, err := m.initiateUnanimousAssertionImpl(request.final)
					if err != nil {
						request.errChan <- err
					} else {
						pendingForProcessing = false
						request.retChan <- ret
					}
				}
			case <-time.After(time.Second):
				if <-m.Val.Bot.HasPendingMessages() {
					// Force onchain assertion if there are pending on chain messages, then force an offchain assertion
					m.initiateUnanimousAssertionImpl(true)
					pendingForProcessing = true
				} else if <-m.mpq.HasMessages() || pendingForProcessing {
					m.initiateUnanimousAssertionImpl(false)
					pendingForProcessing = false
				}
			}
		}
	}()
}

type CoordinatorCreateRequest struct {
	timeout time.Duration
	retChan chan bool
	errChan chan error
}

type CoordinatorDisputableRequest struct {
	retChan chan bool
}

type CoordinatorUnanimousRequest struct {
	final   bool
	retChan chan bool
	errChan chan error
}

func (m *ValidatorCoordinator) CreateVM(timeout time.Duration) (chan bool, chan error) {
	retChan := make(chan bool, 1)
	errChan := make(chan error, 1)
	m.requestChan <- CoordinatorCreateRequest{timeout, retChan, errChan}
	return retChan, errChan
}

func (m *ValidatorCoordinator) InitiateDisputableAssertion() chan bool {
	resChan := make(chan bool, 1)
	m.requestChan <- CoordinatorDisputableRequest{resChan}
	return resChan
}

func (m *ValidatorCoordinator) InitiateUnanimousAssertion(final bool) (chan bool, chan error) {
	retChan := make(chan bool, 1)
	errChan := make(chan error, 1)
	m.requestChan <- CoordinatorUnanimousRequest{final, retChan, errChan}
	return retChan, errChan
}

func (m *ValidatorCoordinator) createVMImpl(timeout time.Duration) (bool, error) {

	gotAll := m.cm.WaitForFollowers(timeout)
	if !gotAll {
		return false, errors.New("coordinator can only create VM when connected to all other validators")
	}

	notifyFollowers := func(allSigned bool) {
		m.cm.broadcast <- &ValidatorRequest{
			Request: &ValidatorRequest_CreateNotification{&CreateVMFinalizedValidatorNotification{
				Approved: allSigned,
			}},
		}
	}
	stateDataChan := m.Val.Bot.RequestVMState()
	stateData := <-stateDataChan
	createData := &CreateVMValidatorRequest{
		Config:              &stateData.Config,
		VmId:                value.NewHashBuf(m.Val.VmId),
		VmState:             value.NewHashBuf(stateData.MachineState),
		ChallengeManagerNum: 0,
	}
	createHash := CreateVMHash(createData)

	responses := m.cm.gatherSignatures(
		&ValidatorRequest{
			Request: &ValidatorRequest_Create{createData},
		},
		createHash,
	)
	if len(responses) != m.Val.ValidatorCount()-1 {
		notifyFollowers(false)
		return false, errors.New("some Validators didn't respond")
	}

	signatures := make([]valmessage.Signature, m.Val.ValidatorCount())
	var err error
	signatures[m.Val.Validators[m.Val.Address()].indexNum], err = m.Val.Sign(createHash)
	if err != nil {
		return false, err
	}
	for _, response := range responses {
		r := response.response.Response.(*FollowerResponse_Create).Create
		if !r.Accepted {
			return false, errors.New("some Validators refused to sign")
		}
		signatures[m.Val.Validators[response.address].indexNum] = valmessage.Signature{
			value.NewHashFromBuf(r.Signature.R),
			value.NewHashFromBuf(r.Signature.S),
			uint8(r.Signature.V),
		}
	}
	_, err = m.Val.CreateVM(createData, signatures)
	return true, err
}

func (m *ValidatorCoordinator) initiateDisputableAssertionImpl() bool {
	start := time.Now()
	resultChan := m.Val.Bot.RequestDisputableAssertion(10000, false)
	res := <-resultChan

	if res {
		log.Printf("Coordinator made disputable assertion in %s seconds", time.Since(start))
	} else {
		log.Printf("Disputable assertion failed")
	}
	return res
}

func (m *ValidatorCoordinator) initiateUnanimousAssertionImpl(forceFinal bool) (bool, error) {
	queuedMessages := <-m.mpq.Fetch()

	isFinal, err := m._initiateUnanimousAssertionImpl(queuedMessages, forceFinal)

	if err != nil {
		m.mpq.Return(queuedMessages)
		return false, err
	}

	if isFinal {
		log.Println("Coordinator is closing unanimous assertion")
		closedChan := m.Val.Bot.CloseUnanimousAssertionRequest()

		closed := <-closedChan
		if closed {
			log.Println("Coordinator successfully closed channel")
		} else {
			log.Println("Coordinator failed to close channel")
		}
		return closed, nil
	} else {
		log.Println("Coordinator is keeping unanimous assertion chain open")
		return true, nil
	}
}

func (m *ValidatorCoordinator) _initiateUnanimousAssertionImpl(queuedMessages []OffchainMessage, forceFinal bool) (bool, error) {
	newMessages := make([]protocol.Message, 0, len(queuedMessages))
	for _, msg := range queuedMessages {
		newMessages = append(newMessages, msg.Message)
	}
	start := time.Now()
	requestChan, resultsChan, unanErrChan := m.Val.Bot.InitiateUnanimousRequest(10000, newMessages, forceFinal)
	responsesChan := make(chan []LabeledFollowerResponse, 1)

	var unanRequest valmessage.UnanimousRequest
	select {
	case unanRequest = <-requestChan:
		break
	case err := <-unanErrChan:
		return false, err
	}

	requestMessages := make([]*SignedMessage, 0, len(unanRequest.NewMessages))
	for i, msg := range unanRequest.NewMessages {
		requestMessages = append(requestMessages, &SignedMessage{
			Message:   protocol.NewMessageBuf(msg),
			Signature: queuedMessages[i].Signature,
		})
	}
	hashId := unanRequest.Hash()

	notifyFollowers := func(msg *UnanimousAssertionValidatorNotification) {
		m.cm.broadcast <- &ValidatorRequest{
			RequestId: value.NewHashBuf(hashId),
			Request:   &ValidatorRequest_UnanimousNotification{msg},
		}
	}

	go func() {
		request := &UnanimousAssertionValidatorRequest{
			BeforeHash:     value.NewHashBuf(unanRequest.BeforeHash),
			BeforeInbox:    value.NewHashBuf(unanRequest.BeforeInbox),
			SequenceNum:    unanRequest.SequenceNum,
			TimeBounds:     protocol.NewTimeBoundsBuf(unanRequest.TimeBounds),
			SignedMessages: requestMessages,
		}
		responsesChan <- m.cm.gatherSignatures(&ValidatorRequest{
			RequestId: value.NewHashBuf(hashId),
			Request: &ValidatorRequest_Unanimous{
				request,
			},
		}, hashId)
	}()

	var unanUpdate valmessage.UnanimousUpdateResults
	select {
	case unanUpdate = <-resultsChan:
		break
	case err := <-unanErrChan:
		notifyFollowers(&UnanimousAssertionValidatorNotification{
			Accepted: false,
		})
		return false, err
	}
	elapsed := time.Since(start)

	// Force onchain assertion if there are outgoing messages
	if len(unanUpdate.Assertion.OutMsgs) > 0 {
		unanUpdate.SequenceNum = math.MaxUint64
	}
	unanHash, err := m.Val.UnanimousAssertHash(
		unanUpdate.SequenceNum,
		unanUpdate.BeforeHash,
		unanUpdate.TimeBounds,
		unanUpdate.NewInboxHash,
		unanUpdate.OriginalInboxHash,
		unanUpdate.Assertion,
	)
	elapsed = time.Since(start)
	if err != nil {
		log.Println("Coordinator failed to hash unanimous assertion")
		notifyFollowers(&UnanimousAssertionValidatorNotification{
			Accepted: false,
		})
		return false, err
	}
	sig, err := m.Val.Sign(unanHash)
	if err != nil {
		log.Println("Coordinator failed to sign unanimous assertion")
		notifyFollowers(&UnanimousAssertionValidatorNotification{
			Accepted: false,
		})
		return false, err
	}

	responses := <-responsesChan
	if len(responses) != m.Val.ValidatorCount()-1 {
		log.Println("Coordinator failed to collect unanimous assertion sigs")
		notifyFollowers(&UnanimousAssertionValidatorNotification{
			Accepted: false,
		})
		return false, errors.New("some Validators didn't respond")
	}

	signatures := make([]valmessage.Signature, m.Val.ValidatorCount())
	rawSignatures := make([]*Signature, m.Val.ValidatorCount())
	signatures[m.Val.Validators[m.Val.Address()].indexNum] = sig
	rawSignatures[m.Val.Validators[m.Val.Address()].indexNum] = &Signature{
		R: value.NewHashBuf(sig.R),
		S: value.NewHashBuf(sig.S),
		V: uint32(sig.V),
	}
	for _, response := range responses {
		r := response.response.Response.(*FollowerResponse_Unanimous).Unanimous
		if !r.Accepted {
			notifyFollowers(&UnanimousAssertionValidatorNotification{
				Accepted: false,
			})
			return false, errors.New("some Validators refused to sign")
		}
		if value.NewHashFromBuf(r.AssertionHash) != unanHash {
			notifyFollowers(&UnanimousAssertionValidatorNotification{
				Accepted: false,
			})
			return false, errors.New("some Validators signed the wrong assertion")
		}
		rawSignatures[m.Val.Validators[response.address].indexNum] = r.Signature
		signatures[m.Val.Validators[response.address].indexNum] = valmessage.Signature{
			value.NewHashFromBuf(r.Signature.R),
			value.NewHashFromBuf(r.Signature.S),
			uint8(r.Signature.V),
		}
	}

	elapsed = time.Since(start)
	log.Printf("Coordinator succeeded signing unanimous assertion in %s\n", elapsed)
	notifyFollowers(&UnanimousAssertionValidatorNotification{
		Accepted:   true,
		Signatures: rawSignatures,
	})

	confRetChan, confErrChan := m.Val.Bot.ConfirmOffchainUnanimousAssertion(
		unanRequest.UnanimousRequestData,
		signatures,
	)

	select {
	case <-confRetChan:
		break
	case err := <-confErrChan:
		return false, err
	}
	return unanUpdate.SequenceNum == math.MaxUint64, nil
}
