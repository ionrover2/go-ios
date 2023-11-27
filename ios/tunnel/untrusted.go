package tunnel

import (
	"bytes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"github.com/danielpaulus/go-ios/ios/opack"
	"github.com/danielpaulus/go-ios/ios/xpc"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/ed25519"
	"golang.org/x/crypto/hkdf"
	"io"
)

const UntrustedTunnelServiceName = "com.apple.internal.dt.coredevice.untrusted.tunnelservice"

func NewTunnelServiceWithXpc(xpcConn *xpc.Connection, c io.Closer) (*TunnelService, error) {
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &TunnelService{xpcConn: xpcConn, c: c, key: key, messageReadWriter: newControlChannelCodec()}, nil
}

func NewTunnelServiceWithSessionKey(conn *xpc.Connection, c io.Closer, sessionKey []byte) (*TunnelService, error) {
	ts := &TunnelService{
		xpcConn:           conn,
		c:                 c,
		key:               nil,
		messageReadWriter: newControlChannelCodec(),
	}
	err := ts.setupCiphers(sessionKey)
	if err != nil {
		return nil, err
	}
	return ts, nil
}

type TunnelService struct {
	xpcConn *xpc.Connection
	c       io.Closer
	key     *ecdh.PrivateKey

	clientEncryption  cipher.AEAD
	serverEncryption  cipher.AEAD
	cs                *cipherStream
	messageReadWriter *controlChannelCodec
}

type PairInfo struct {
	SessionKey []byte
}

func (t *TunnelService) Close() error {
	return t.c.Close()
}

func (t *TunnelService) Pair() (PairInfo, error) {
	err := t.xpcConn.Send(EncodeRequest(t.messageReadWriter, map[string]interface{}{
		"handshake": map[string]interface{}{
			"_0": map[string]interface{}{
				"hostOptions": map[string]interface{}{
					"attemptPairVerify": false,
				},
				"wireProtocolVersion": int64(19),
			},
		},
	}))

	if err != nil {
		return PairInfo{}, err
	}
	m, err := t.xpcConn.ReceiveOnClientServerStream()
	if err != nil {
		return PairInfo{}, err
	}
	m, err = t.messageReadWriter.Decode(m)

	err = t.setupManualPairing()
	if err != nil {
		return PairInfo{}, err
	}

	devPublicKey, devSaltKey, err := t.readDeviceKey()
	if err != nil {
		return PairInfo{}, err
	}

	srp, err := NewSrpInfo(devSaltKey, devPublicKey)
	if err != nil {
		return PairInfo{}, err
	}

	proofTlv := NewTlvBuffer()
	proofTlv.WriteByte(TypeState, PairStateVerifyRequest)
	proofTlv.WriteData(TypePublicKey, srp.ClientPublic)
	proofTlv.WriteData(TypeProof, srp.ClientProof)

	err = t.xpcConn.Send(EncodeEvent(t.messageReadWriter, &pairingData{
		data: proofTlv.Bytes(),
		kind: "setupManualPairing",
	}))
	if err != nil {
		return PairInfo{}, err
	}

	m, err = t.xpcConn.ReceiveOnClientServerStream()
	if err != nil {
		return PairInfo{}, err
	}

	var proofPairingData pairingData
	DecodeEvent(t.messageReadWriter, m, &proofPairingData)

	serverProof, err := TlvReader(proofPairingData.data).ReadCoalesced(TypeProof)
	if err != nil {
		return PairInfo{}, err
	}
	verified := srp.VerifyServerProof(serverProof)
	if !verified {
		return PairInfo{}, fmt.Errorf("could not verify server proof")
	}

	identifier := uuid.New()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	hkdfPairSetup := hkdf.New(sha512.New, srp.SessionKey, []byte("Pair-Setup-Controller-Sign-Salt"), []byte("Pair-Setup-Controller-Sign-Info"))
	buf := bytes.NewBuffer(nil)
	io.CopyN(buf, hkdfPairSetup, 32)
	buf.WriteString(identifier.String())
	buf.Write(public)

	if err != nil {
		return PairInfo{}, err
	}
	signature := ed25519.Sign(private, buf.Bytes())

	deviceInfo, err := opack.Encode(map[string]interface{}{
		"accountID":                   identifier.String(),
		"altIRK":                      []byte{0x5e, 0xca, 0x81, 0x91, 0x92, 0x02, 0x82, 0x00, 0x11, 0x22, 0x33, 0x44, 0xbb, 0xf2, 0x4a, 0xc8},
		"btAddr":                      "FF:DD:99:66:BB:AA",
		"mac":                         []byte{0xff, 0x44, 0x88, 0x66, 0x33, 0x99},
		"model":                       "MacBookPro18,3",
		"name":                        "host-name",
		"remotepairing_serial_number": "YY9944YY99",
	})

	deviceInfoTlv := NewTlvBuffer()
	deviceInfoTlv.WriteData(TypeSignature, signature)
	deviceInfoTlv.WriteData(TypePublicKey, public)
	deviceInfoTlv.WriteData(TypeIdentifier, []byte(identifier.String()))
	deviceInfoTlv.WriteData(TypeInfo, deviceInfo)

	sessionKeyBuf := bytes.NewBuffer(nil)
	_, err = io.CopyN(sessionKeyBuf, hkdf.New(sha512.New, srp.SessionKey, []byte("Pair-Setup-Encrypt-Salt"), []byte("Pair-Setup-Encrypt-Info")), 32)
	if err != nil {
		return PairInfo{}, err
	}
	setupKey := sessionKeyBuf.Bytes()

	cipher, err := chacha20poly1305.New(setupKey)
	if err != nil {
		return PairInfo{}, err
	}

	nonce := make([]byte, cipher.NonceSize())
	copy(nonce[4:], "PS-Msg05")
	x := cipher.Seal(nil, nonce, deviceInfoTlv.Bytes(), nil)

	encryptedTlv := NewTlvBuffer()
	encryptedTlv.WriteByte(TypeState, 0x05)
	encryptedTlv.WriteData(TypeEncryptedData, x)

	err = t.xpcConn.Send(EncodeEvent(t.messageReadWriter, &pairingData{
		data:        encryptedTlv.Bytes(),
		kind:        "setupManualPairing",
		sendingHost: "SL-1876",
	}))
	if err != nil {
		return PairInfo{}, err
	}

	m, err = t.xpcConn.ReceiveOnClientServerStream()
	if err != nil {
		return PairInfo{}, err
	}

	var encRes pairingData
	err = DecodeEvent(t.messageReadWriter, m, &encRes)
	if err != nil {
		return PairInfo{}, err
	}

	encrData, err := TlvReader(encRes.data).ReadCoalesced(TypeEncryptedData)
	if err != nil {
		return PairInfo{}, err
	}
	copy(nonce[4:], "PS-Msg06")
	// the device info response from the device is not needed. we just make sure that there's no error decrypting it
	_, err = cipher.Open(nil, nonce, encrData, nil)
	if err != nil {
		return PairInfo{}, err
	}

	err = t.setupCiphers(srp.SessionKey)
	if err != nil {
		return PairInfo{}, err
	}

	_, err = t.createUnlockKey()
	if err != nil {
		return PairInfo{}, err
	}

	return PairInfo{SessionKey: srp.SessionKey}, nil
}

func (t *TunnelService) CreateTunnelListener() (TunnelListener, error) {
	log.Info("create tunnel listener")
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)

	if err != nil {
		return TunnelListener{}, err
	}
	der, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return TunnelListener{}, err
	}

	createListenerRequest, err := EncodeStreamEncrypted(t.messageReadWriter, t.clientEncryption, t.cs, map[string]interface{}{
		"request": map[string]interface{}{
			"_0": map[string]interface{}{
				"createListener": map[string]interface{}{
					"key":                   der,
					"transportProtocolType": "quic",
					//"transportProtocolType": "tcp",
				},
			},
		},
	})
	if err != nil {
		return TunnelListener{}, err
	}
	err = t.xpcConn.Send(createListenerRequest)
	if err != nil {
		return TunnelListener{}, err
	}

	m, err := t.xpcConn.ReceiveOnClientServerStream()
	if err != nil {
		return TunnelListener{}, err
	}

	listenerRes, err := DecodeStreamEncrypted(t.messageReadWriter, t.serverEncryption, t.cs, m)
	if err != nil {
		return TunnelListener{}, err
	}

	createListener, err := getChildMap(listenerRes, "response", "_1", "createListener")
	if err != nil {
		return TunnelListener{}, err
	}
	port := createListener["port"].(float64)
	devPublicKey := createListener["devicePublicKey"].(string)
	devPK, err := base64.StdEncoding.DecodeString(devPublicKey)
	if err != nil {
		return TunnelListener{}, err
	}
	publicKey, err := x509.ParsePKIXPublicKey(devPK)
	if err != nil {
		return TunnelListener{}, err
	}
	return TunnelListener{
		PrivateKey:      privateKey,
		DevicePublicKey: publicKey,
		TunnelPort:      uint64(port),
	}, nil
}

func (t *TunnelService) setupCiphers(sessionKey []byte) error {
	clientKey := make([]byte, 32)
	_, err := hkdf.New(sha512.New, sessionKey, nil, []byte("ClientEncrypt-main")).Read(clientKey)
	if err != nil {
		return err
	}
	serverKey := make([]byte, 32)
	_, err = hkdf.New(sha512.New, sessionKey, nil, []byte("ServerEncrypt-main")).Read(serverKey)
	if err != nil {
		return err
	}
	t.serverEncryption, err = chacha20poly1305.New(serverKey)
	if err != nil {
		return err
	}
	t.clientEncryption, err = chacha20poly1305.New(clientKey)
	if err != nil {
		return err
	}
	t.cs = &cipherStream{}
	return nil
}

func (t *TunnelService) setupManualPairing() error {
	buf := NewTlvBuffer()
	buf.WriteByte(TypeMethod, 0x00)
	buf.WriteByte(TypeState, PairStateStartRequest)

	event := pairingData{
		data:            buf.Bytes(),
		kind:            "setupManualPairing",
		startNewSession: true,
	}

	err := t.xpcConn.Send(EncodeEvent(t.messageReadWriter, &event))
	if err != nil {
		return err
	}
	res, err := t.xpcConn.ReceiveOnClientServerStream()
	if err != nil {
		return err
	}
	_, err = t.messageReadWriter.Decode(res)
	return err
}

func (t *TunnelService) readDeviceKey() (publicKey []byte, salt []byte, err error) {
	m, err := t.xpcConn.ReceiveOnClientServerStream()
	if err != nil {
		return
	}
	var pairingData pairingData
	err = DecodeEvent(t.messageReadWriter, m, &pairingData)
	if err != nil {
		return
	}
	publicKey, err = TlvReader(pairingData.data).ReadCoalesced(TypePublicKey)
	if err != nil {
		return
	}
	salt, err = TlvReader(pairingData.data).ReadCoalesced(TypeSalt)
	if err != nil {
		return
	}
	return
}

func (t *TunnelService) createUnlockKey() ([]byte, error) {
	unlockReqMsg, err := EncodeStreamEncrypted(t.messageReadWriter, t.clientEncryption, t.cs, map[string]interface{}{
		"request": map[string]interface{}{
			"_0": map[string]interface{}{
				"createRemoteUnlockKey": map[string]interface{}{},
			},
		},
	})

	err = t.xpcConn.Send(unlockReqMsg)
	if err != nil {
		return nil, err
	}

	m, err := t.xpcConn.ReceiveOnClientServerStream()
	if err != nil {
		return nil, err
	}

	_, _ = DecodeStreamEncrypted(t.messageReadWriter, t.serverEncryption, t.cs, m)
	return nil, err
}

type TunnelListener struct {
	PrivateKey      *rsa.PrivateKey
	DevicePublicKey interface{}
	TunnelPort      uint64
}

type TunnelInfo struct {
	ServerAddress    string
	ServerRSDPort    uint64
	ClientParameters struct {
		Address string
		Netmask string
		Mtu     uint64
	}
}
