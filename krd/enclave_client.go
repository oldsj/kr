package main

/*
*	Facillitates communication with a mobile phone SSH key enclave.
 */

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agrinman/kr"
	"github.com/golang/groupcache/lru"
	"sync"
	"time"
)

var ErrTimeout = errors.New("Request timed out")

//	Message queued during send
type SendQueued struct {
	error
}

func (err *SendQueued) Error() string {
	return fmt.Sprintf("SendQueued: " + err.error.Error())
}

//	Network-related error during send
type SendError struct {
	error
}

func (err *SendError) Error() string {
	return fmt.Sprintf("SendError: " + err.error.Error())
}

//	Network-related error during receive
type RecvError struct {
	error
}

func (err *RecvError) Error() string {
	return fmt.Sprintf("RecvError: " + err.error.Error())
}

//	Unrecoverable error, this request will always fail
type ProtoError struct {
	error
}

func (err *ProtoError) Error() string {
	return fmt.Sprintf("ProtoError: " + err.error.Error())
}

type EnclaveClientI interface {
	Pair() (pairing kr.PairingSecret, err error)
	IsPaired() bool
	Start() (err error)
	Stop() (err error)
	RequestMe() (*kr.MeResponse, error)
	GetCachedMe() *kr.Profile
	RequestSignature(kr.SignRequest) (*kr.SignResponse, error)
	RequestList(kr.ListRequest) (*kr.ListResponse, error)
}

type EnclaveClient struct {
	sync.Mutex
	pairingSecret               *kr.PairingSecret
	requestCallbacksByRequestID *lru.Cache
	outgoingQueue               [][]byte
	snsEndpointARN              *string
	cachedMe                    *kr.Profile
	bt                          BluetoothDriverI
}

func (ec *EnclaveClient) Pair() (pairingSecret kr.PairingSecret, err error) {
	ec.Lock()
	defer ec.Unlock()
	ec.cachedMe = nil

	ec.deactivatePairing()
	ec.generatePairing()
	ec.activatePairing()

	pairingSecret = *ec.pairingSecret

	return
}

func (ec *EnclaveClient) IsPaired() bool {
	ps := ec.getPairingSecret()
	if ps == nil {
		return false
	}
	return ps.IsPaired()
}

func (ec *EnclaveClient) generatePairing() (err error) {
	ec.deactivatePairing()
	kr.DeletePairing()

	pairingSecret, err := kr.GeneratePairingSecretAndCreateQueues()
	if err != nil {
		log.Error(err)
		return
	}
	//	erase any existing pairing
	ec.pairingSecret = &pairingSecret
	ec.outgoingQueue = [][]byte{}
	pairingSecret = *ec.pairingSecret

	savePairingErr := kr.SavePairing(pairingSecret)
	if savePairingErr != nil {
		log.Error("error saving pairing:", savePairingErr.Error())
	}
	return
}

func (ec *EnclaveClient) deactivatePairing() (err error) {
	if ec.bt != nil {
		if ec.pairingSecret != nil {
			oldBtUUID, uuidErr := ec.pairingSecret.DeriveUUID()
			if uuidErr == nil {
				btErr := ec.bt.RemoveService(oldBtUUID)
				if btErr != nil {
					log.Error("error removing bluetooth service:", btErr.Error())
				}
			}
		}
	}
	return
}

func (ec *EnclaveClient) activatePairing() (err error) {
	if ec.bt != nil {
		if ec.pairingSecret != nil {
			btUUID, uuidErr := ec.pairingSecret.DeriveUUID()
			if uuidErr != nil {
				err = uuidErr
				log.Error(err)
				return
			}
			err = ec.bt.AddService(btUUID)
			if err != nil {
				log.Error(err)
				return
			}
		}
	}
	return
}
func (ec *EnclaveClient) Stop() (err error) {
	ec.deactivatePairing()
	return
}

func (ec *EnclaveClient) Start() (err error) {
	ec.Lock()
	loadedPairing, loadErr := kr.LoadPairing()
	if loadErr == nil && loadedPairing != nil {
		ec.pairingSecret = loadedPairing
	} else {
		log.Notice("pairing not loaded:", loadErr)
	}

	bt, err := NewBluetoothDriver()
	if err != nil {
		log.Error("error starting bluetooth driver:", err)
	} else {
		ec.bt = bt
		go func() {
			readChan, err := ec.bt.ReadChan()
			if err != nil {
				log.Error("error retrieving bluetooth read channel:", err)
				return
			}
			for ciphertext := range readChan {
				err = ec.handleCiphertext(ciphertext)
			}
		}()
	}

	ec.activatePairing()
	ec.Unlock()
	if ec.getPairingSecret() != nil {
		ec.RequestMe()
	}
	return
}

func (ec *EnclaveClient) getPairingSecret() (pairingSecret *kr.PairingSecret) {
	ec.Lock()
	defer ec.Unlock()
	pairingSecret = ec.pairingSecret
	return
}

func (ec *EnclaveClient) GetCachedMe() (me *kr.Profile) {
	ec.Lock()
	defer ec.Unlock()
	me = ec.cachedMe
	return
}

func UnpairedEnclaveClient() EnclaveClientI {
	return &EnclaveClient{
		requestCallbacksByRequestID: lru.New(128),
	}
}

func (client *EnclaveClient) RequestMe() (meResponse *kr.MeResponse, err error) {
	meRequest, err := kr.NewRequest()
	if err != nil {
		log.Error(err)
		return
	}
	meRequest.MeRequest = &kr.MeRequest{}
	response, err := client.tryRequest(meRequest, 20*time.Second)
	if err != nil {
		log.Error(err)
		return
	}
	if response != nil {
		meResponse = response.MeResponse
		if meResponse != nil {
			client.Lock()
			client.cachedMe = &meResponse.Me
			client.Unlock()
		}
	}
	return
}
func (client *EnclaveClient) RequestSignature(signRequest kr.SignRequest) (signResponse *kr.SignResponse, err error) {
	start := time.Now()
	request, err := kr.NewRequest()
	if err != nil {
		log.Error(err)
		return
	}
	request.SignRequest = &signRequest
	response, err := client.tryRequest(request, 15*time.Second)
	if err != nil {
		log.Error(err)
		return
	}
	if response != nil {
		signResponse = response.SignResponse
		log.Notice("successful signature took", int(time.Since(start)/time.Millisecond), "ms")
	}
	return
}
func (client *EnclaveClient) RequestList(listRequest kr.ListRequest) (listResponse *kr.ListResponse, err error) {
	request, err := kr.NewRequest()
	if err != nil {
		log.Error(err)
		return
	}
	request.ListRequest = &listRequest
	response, err := client.tryRequest(request, 5*time.Second)
	if err != nil {
		log.Error(err)
		return
	}
	if response != nil {
		listResponse = response.ListResponse
	}
	return
}

func (client *EnclaveClient) tryRequest(request kr.Request, timeout time.Duration) (response *kr.Response, err error) {
	cb := make(chan *kr.Response, 1)
	go func() {
		err := client.sendRequestAndReceiveResponses(request, cb, timeout)
		if err != nil {
			log.Error("error sendRequestAndReceiveResponses: ", err.Error())
		}
	}()
	select {
	case response = <-cb:
	case <-time.After(timeout):
		err = ErrTimeout
	}
	return
}

//	Send one request and receive pending responses, not necessarily associated
//	with this request
func (client *EnclaveClient) sendRequestAndReceiveResponses(request kr.Request, cb chan *kr.Response, timeout time.Duration) (err error) {
	pairingSecret := client.getPairingSecret()
	if pairingSecret == nil {
		err = errors.New("EnclaveClient pairing never initiated")
		return
	}
	requestJson, err := json.Marshal(request)
	if err != nil {
		err = &ProtoError{err}
		return
	}

	timeoutAt := time.Now().Add(timeout)

	client.Lock()
	client.requestCallbacksByRequestID.Add(request.RequestID, cb)
	client.Unlock()

	err = client.sendMessage(requestJson)

	if err != nil {
		switch err.(type) {
		case *SendQueued:
			log.Notice(err)
			err = nil
		default:
			return
		}
	}

	receive := func() (numReceived int, err error) {
		ciphertexts, err := pairingSecret.ReadQueue()
		if err != nil {
			err = &RecvError{err}
			return
		}

		for _, ctxt := range ciphertexts {
			ctxtErr := client.handleCiphertext(ctxt)
			switch ctxtErr {
			case kr.ErrWaitingForKey:
			default:
				err = ctxtErr
			}
		}
		return
	}

	for {
		n, err := receive()
		_, requestPending := client.requestCallbacksByRequestID.Get(request.RequestID)
		if err != nil || (n == 0 && time.Now().After(timeoutAt)) || !requestPending {
			if err != nil {
				log.Error("queue err:", err)
			}
			break
		}
	}
	client.Lock()
	if cb, ok := client.requestCallbacksByRequestID.Get(request.RequestID); ok {
		//	request still not processed, give up on it
		cb.(chan *kr.Response) <- nil
		client.requestCallbacksByRequestID.Remove(request.RequestID)
		log.Error("evicting request", request.RequestID)
	}
	client.Unlock()

	return
}

func (client *EnclaveClient) handleCiphertext(ciphertext []byte) (err error) {
	pairingSecret := client.getPairingSecret()
	unwrappedCiphertext, didUnwrapKey, err := pairingSecret.UnwrapKeyIfPresent(ciphertext)
	if pairingSecret == nil {
		err = errors.New("EnclaveClient pairing never initiated")
		return
	}
	if err != nil {
		err = &ProtoError{err}
		return
	}
	if didUnwrapKey {
		client.Lock()
		queue := client.outgoingQueue
		client.outgoingQueue = [][]byte{}
		client.Unlock()

		savePairingErr := kr.SavePairing(*pairingSecret)
		if savePairingErr != nil {
			log.Error("error saving pairing:", savePairingErr.Error())
		}

		for _, queuedMessage := range queue {
			err = client.sendMessage(queuedMessage)
			if err != nil {
				log.Error("error sending queued message:", err.Error())
			}
		}
	}
	if unwrappedCiphertext == nil {
		return
	}
	message, err := pairingSecret.DecryptMessage(*unwrappedCiphertext)
	if err != nil {
		log.Error("decrypt error:", err)
		return
	}
	if message == nil {
		return
	}
	responseJson := *message
	err = client.handleMessage(responseJson)
	if err != nil {
		log.Error("handleMessage error:", err)
		return
	}
	return
}

func (client *EnclaveClient) sendMessage(message []byte) (err error) {
	pairingSecret := client.getPairingSecret()
	if pairingSecret == nil {
		err = errors.New("EnclaveClient pairing never initiated")
		return
	}
	ciphertext, err := pairingSecret.EncryptMessage(message)
	if err != nil {
		if err == kr.ErrWaitingForKey {
			client.Lock()
			if len(client.outgoingQueue) < 128 {
				client.outgoingQueue = append(client.outgoingQueue, message)
			}
			client.Unlock()
			err = &SendQueued{err}
		} else {
			err = &SendError{err}
		}
		return
	}
	go func() {
		err := client.bt.Write(ciphertext)
		if err != nil {
			log.Error("error writing BT", err)
		}
	}()

	err = pairingSecret.SendMessage(message)
	if err != nil {
		err = &SendError{err}
		return
	}
	return
}

func (client *EnclaveClient) handleMessage(message []byte) (err error) {
	var response kr.Response
	err = json.Unmarshal(message, &response)
	if err != nil {
		return
	}

	if response.SNSEndpointARN != nil {
		client.Lock()
		if client.pairingSecret != nil {
			client.pairingSecret.SetSNSEndpointARN(response.SNSEndpointARN)
			kr.SavePairing(*client.pairingSecret)
		}
		client.Unlock()
	}

	client.Lock()
	if requestCb, ok := client.requestCallbacksByRequestID.Get(response.RequestID); ok {
		log.Info("found callback for request", response.RequestID)
		requestCb.(chan *kr.Response) <- &response
	} else {
		log.Info("callback not found for request", response.RequestID)
	}
	client.requestCallbacksByRequestID.Remove(response.RequestID)
	client.Unlock()
	return
}