package biometricvault

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"math/big"
	"time"

	"github.com/godbus/dbus/v5"

	"proactive-interaction-engine/internal/domain/fault"
)

const (
	secretServiceBusName       = "org.freedesktop.secrets"
	secretServicePath          = dbus.ObjectPath("/org/freedesktop/secrets")
	secretServiceInterface     = "org.freedesktop.Secret.Service"
	secretCollectionInterface  = "org.freedesktop.Secret.Collection"
	secretItemInterface        = "org.freedesktop.Secret.Item"
	secretSessionInterface     = "org.freedesktop.Secret.Session"
	secretPromptInterface      = "org.freedesktop.Secret.Prompt"
	propertiesInterface        = "org.freedesktop.DBus.Properties"
	secretServiceAlgorithm     = "dh-ietf1024-sha256-aes128-cbc-pkcs7"
	secretServiceContentType   = "application/octet-stream"
	secretServiceItemLabel     = "Proactive Interaction Engine biometric vault master key"
	secretServiceCloseDeadline = 250 * time.Millisecond
)

type dbusSecretServiceFactory struct {
	random io.Reader
}

type secretServiceConnection interface {
	Object(string, dbus.ObjectPath) dbus.BusObject
	Close() error
}

func (f dbusSecretServiceFactory) Open(ctx context.Context) (secretServiceStore, error) {
	if err := validateContext("open Secret Service session", ctx); err != nil {
		return nil, err
	}
	if isNil(f.random) {
		return nil, fault.New(fault.InvalidInput, "open Secret Service session", errors.New("entropy source is required"))
	}
	connection, err := dbus.ConnectSessionBus(dbus.WithContext(ctx))
	if err != nil {
		return nil, classifySecretServiceFault("connect to Secret Service", err)
	}
	store := &dbusSecretServiceStore{connection: connection, random: f.random}
	if err := store.openSession(ctx); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return store, nil
}

type dbusSecretServiceStore struct {
	connection secretServiceConnection
	random     io.Reader
	session    dbus.ObjectPath
	aesKey     [aes.BlockSize]byte
}

type secretValue struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

func (s *dbusSecretServiceStore) openSession(ctx context.Context) error {
	const op = "open encrypted Secret Service session"
	private, public, err := newDHKeyPair(s.random)
	if err != nil {
		return fault.New(fault.Unavailable, op, fmt.Errorf("generate DH key pair: %w", err))
	}
	defer private.SetInt64(0)
	var output dbus.Variant
	var session dbus.ObjectPath
	call := s.service().CallWithContext(ctx, secretServiceInterface+".OpenSession", 0,
		secretServiceAlgorithm, dbus.MakeVariant(public))
	if err := call.Store(&output, &session); err != nil {
		return classifySecretServiceFault(op, err)
	}
	peerPublic, ok := output.Value().([]byte)
	if !ok || !validObjectPath(session) {
		return fault.New(fault.AdapterRejected, op, errors.New("Secret Service returned an invalid encrypted session"))
	}
	key, err := deriveDHSessionKey(private, peerPublic)
	if err != nil {
		return fault.New(fault.AdapterRejected, op, err)
	}
	s.session = session
	s.aesKey = key
	return nil
}

func (s *dbusSecretServiceStore) Lookup(ctx context.Context) (MasterKey, bool, error) {
	const op = "look up Secret Service biometric master key"
	var unlocked, locked []dbus.ObjectPath
	call := s.service().CallWithContext(ctx, secretServiceInterface+".SearchItems", 0, cloneAttributes())
	if err := call.Store(&unlocked, &locked); err != nil {
		return MasterKey{}, false, classifySecretServiceFault(op, err)
	}
	if len(locked) != 0 {
		return MasterKey{}, false, fault.New(fault.PermissionDenied, op, errors.New("matching Secret Service item is locked"))
	}
	if len(unlocked) == 0 {
		return MasterKey{}, false, nil
	}
	if len(unlocked) != 1 || !validObjectPath(unlocked[0]) {
		return MasterKey{}, false, fault.New(fault.AdapterRejected, op, errors.New("Secret Service returned ambiguous key items"))
	}
	var secret secretValue
	call = s.object(unlocked[0]).CallWithContext(ctx, secretItemInterface+".GetSecret", 0, s.session)
	if err := call.Store(&secret); err != nil {
		return MasterKey{}, false, classifySecretServiceFault(op, err)
	}
	plain, err := decryptSecretValue(s.aesKey, s.session, secret)
	if err != nil {
		return MasterKey{}, false, fault.New(fault.AdapterRejected, op, err)
	}
	defer clear(plain)
	if len(plain) != len(MasterKey{}) {
		return MasterKey{}, false, fault.New(fault.AdapterRejected, op, errors.New("Secret Service key has invalid length"))
	}
	var key MasterKey
	copy(key[:], plain)
	if isZeroMasterKey(key) {
		return MasterKey{}, false, fault.New(fault.AdapterRejected, op, errors.New("Secret Service returned a zero key"))
	}
	return key, true, nil
}

func (s *dbusSecretServiceStore) Create(ctx context.Context, key MasterKey) error {
	const op = "create Secret Service biometric master key"
	if isZeroMasterKey(key) {
		return fault.New(fault.InvalidInput, op, errors.New("master key must be nonzero"))
	}
	var collection dbus.ObjectPath
	call := s.service().CallWithContext(ctx, secretServiceInterface+".ReadAlias", 0, "default")
	if err := call.Store(&collection); err != nil {
		return classifySecretServiceFault(op, err)
	}
	if !validObjectPath(collection) {
		return fault.New(fault.Unavailable, op, errors.New("Secret Service default collection is unavailable"))
	}
	locked, err := s.collectionLocked(ctx, collection)
	if err != nil {
		return err
	}
	if locked {
		return fault.New(fault.PermissionDenied, op, errors.New("Secret Service default collection is locked"))
	}
	secret, err := encryptSecretValue(s.random, s.aesKey, s.session, key[:])
	if err != nil {
		return fault.New(fault.Unavailable, op, fmt.Errorf("encrypt master key for Secret Service: %w", err))
	}
	defer clear(secret.Value)
	properties := map[string]dbus.Variant{
		secretItemInterface + ".Label":      dbus.MakeVariant(secretServiceItemLabel),
		secretItemInterface + ".Attributes": dbus.MakeVariant(cloneAttributes()),
	}
	var item, prompt dbus.ObjectPath
	call = s.object(collection).CallWithContext(ctx, secretCollectionInterface+".CreateItem", 0,
		properties, secret, false)
	if err := call.Store(&item, &prompt); err != nil {
		return classifySecretServiceFault(op, err)
	}
	if prompt != "/" {
		s.dismissPrompt(prompt)
		return fault.New(fault.PermissionDenied, op, errors.New("Secret Service requested interactive authorization"))
	}
	if !validObjectPath(item) {
		return fault.New(fault.AdapterRejected, op, errors.New("Secret Service returned an invalid created item"))
	}
	return nil
}

func (s *dbusSecretServiceStore) collectionLocked(ctx context.Context, collection dbus.ObjectPath) (bool, error) {
	const op = "inspect Secret Service collection"
	var property dbus.Variant
	call := s.object(collection).CallWithContext(ctx, propertiesInterface+".Get", 0,
		secretCollectionInterface, "Locked")
	if err := call.Store(&property); err != nil {
		return false, classifySecretServiceFault(op, err)
	}
	locked, ok := property.Value().(bool)
	if !ok {
		return false, fault.New(fault.AdapterRejected, op, errors.New("Secret Service returned an invalid Locked property"))
	}
	return locked, nil
}

func (s *dbusSecretServiceStore) dismissPrompt(prompt dbus.ObjectPath) {
	if !validObjectPath(prompt) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), secretServiceCloseDeadline)
	defer cancel()
	_ = s.object(prompt).CallWithContext(ctx, secretPromptInterface+".Dismiss", 0).Err
}

func (s *dbusSecretServiceStore) Close() error {
	if s == nil || s.connection == nil {
		return nil
	}
	if validObjectPath(s.session) {
		ctx, cancel := context.WithTimeout(context.Background(), secretServiceCloseDeadline)
		_ = s.object(s.session).CallWithContext(ctx, secretSessionInterface+".Close", 0).Err
		cancel()
	}
	clear(s.aesKey[:])
	err := s.connection.Close()
	s.connection = nil
	return err
}

func (s *dbusSecretServiceStore) service() dbus.BusObject {
	return s.object(secretServicePath)
}

func (s *dbusSecretServiceStore) object(path dbus.ObjectPath) dbus.BusObject {
	return s.connection.Object(secretServiceBusName, path)
}

func validObjectPath(path dbus.ObjectPath) bool {
	return path != "" && path != "/" && path.IsValid()
}

func cloneAttributes() map[string]string {
	return map[string]string{
		"application": "proactive-interaction-engine",
		"purpose":     "biometric-vault-master-key",
		"version":     "1",
		"xdg:schema":  "org.proactiveinteractionengine.BiometricMasterKey",
	}
}

func newDHKeyPair(random io.Reader) (*big.Int, []byte, error) {
	if isNil(random) {
		return nil, nil, errors.New("entropy source is required")
	}
	prime := secretServiceDHPrime()
	limit := new(big.Int).Sub(prime, big.NewInt(3))
	private, err := rand.Int(random, limit)
	if err != nil {
		return nil, nil, err
	}
	private.Add(private, big.NewInt(2))
	public := new(big.Int).Exp(big.NewInt(2), private, prime)
	return private, public.Bytes(), nil
}

func deriveDHSessionKey(private *big.Int, peerPublicBytes []byte) ([aes.BlockSize]byte, error) {
	var key [aes.BlockSize]byte
	if private == nil || private.Sign() <= 0 || len(peerPublicBytes) == 0 || len(peerPublicBytes) > 128 {
		return key, errors.New("invalid Secret Service DH key exchange")
	}
	prime := secretServiceDHPrime()
	peer := new(big.Int).SetBytes(peerPublicBytes)
	upper := new(big.Int).Sub(prime, big.NewInt(2))
	if peer.Cmp(big.NewInt(2)) < 0 || peer.Cmp(upper) > 0 {
		return key, errors.New("Secret Service returned an invalid DH public key")
	}
	shared := new(big.Int).Exp(peer, private, prime)
	sharedBytes := shared.FillBytes(make([]byte, (prime.BitLen()+7)/8))
	defer clear(sharedBytes)
	derived := hkdfSHA256(sharedBytes, aes.BlockSize)
	copy(key[:], derived)
	clear(derived)
	return key, nil
}

func secretServiceDHPrime() *big.Int {
	const encoded = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD1" +
		"29024E088A67CC74020BBEA63B139B22514A08798E3404DD" +
		"EF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245" +
		"E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7ED" +
		"EE386BFB5A899FA5AE9F24117C4B1FE649286651ECE65381" +
		"FFFFFFFFFFFFFFFF"
	prime, ok := new(big.Int).SetString(encoded, 16)
	if !ok {
		panic("invalid fixed Secret Service DH prime")
	}
	return prime
}

func hkdfSHA256(secret []byte, length int) []byte {
	salt := make([]byte, sha256.Size)
	extract := hmac.New(sha256.New, salt)
	_, _ = extract.Write(secret)
	prk := extract.Sum(nil)
	defer clear(prk)
	output := make([]byte, 0, length)
	previous := []byte(nil)
	for counter := byte(1); len(output) < length; counter++ {
		expand := hmac.New(sha256.New, prk)
		_, _ = expand.Write(previous)
		_, _ = expand.Write([]byte{counter})
		previous = expand.Sum(previous[:0])
		output = append(output, previous...)
	}
	clear(previous)
	return output[:length]
}

func encryptSecretValue(random io.Reader, key [aes.BlockSize]byte, session dbus.ObjectPath, plain []byte) (secretValue, error) {
	if !validObjectPath(session) || isNil(random) {
		return secretValue{}, errors.New("valid encrypted Secret Service session is required")
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return secretValue{}, err
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(random, iv); err != nil {
		return secretValue{}, err
	}
	padding := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, len(plain)+padding)
	copy(padded, plain)
	for index := len(plain); index < len(padded); index++ {
		padded[index] = byte(padding)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(padded, padded)
	return secretValue{Session: session, Parameters: iv, Value: padded, ContentType: secretServiceContentType}, nil
}

func decryptSecretValue(key [aes.BlockSize]byte, session dbus.ObjectPath, secret secretValue) ([]byte, error) {
	if secret.Session != session || !validObjectPath(session) || secret.ContentType != secretServiceContentType {
		return nil, errors.New("Secret Service returned mismatched secret metadata")
	}
	if len(secret.Parameters) != aes.BlockSize || len(secret.Value) == 0 || len(secret.Value)%aes.BlockSize != 0 {
		return nil, errors.New("Secret Service returned invalid encrypted secret framing")
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	plain := bytes.Clone(secret.Value)
	cipher.NewCBCDecrypter(block, secret.Parameters).CryptBlocks(plain, plain)
	padding := int(plain[len(plain)-1])
	if padding == 0 || padding > aes.BlockSize || padding > len(plain) {
		clear(plain)
		return nil, errors.New("Secret Service returned invalid encrypted secret padding")
	}
	valid := 1
	for index := len(plain) - padding; index < len(plain); index++ {
		valid &= subtle.ConstantTimeByteEq(plain[index], byte(padding))
	}
	if valid != 1 {
		clear(plain)
		return nil, errors.New("Secret Service returned invalid encrypted secret padding")
	}
	result := bytes.Clone(plain[:len(plain)-padding])
	clear(plain)
	return result, nil
}

func classifySecretServiceFault(op string, err error) error {
	if err == nil {
		return nil
	}
	var typed *fault.Error
	if errors.As(err, &typed) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fault.New(fault.DeadlineExceeded, op, context.DeadlineExceeded)
	}
	if errors.Is(err, context.Canceled) {
		return fault.New(fault.Unavailable, op, context.Canceled)
	}
	name := dbusErrorName(err)
	switch name {
	case "org.freedesktop.Secret.Error.IsLocked", "org.freedesktop.DBus.Error.AccessDenied", "org.freedesktop.DBus.Error.AuthFailed":
		return fault.New(fault.PermissionDenied, op, errors.New("Secret Service denied access or is locked"))
	case "org.freedesktop.DBus.Error.NotSupported", "org.freedesktop.DBus.Error.ServiceUnknown",
		"org.freedesktop.DBus.Error.NameHasNoOwner", "org.freedesktop.DBus.Error.NoReply",
		"org.freedesktop.DBus.Error.Disconnected":
		return fault.New(fault.Unavailable, op, errors.New("Secret Service is unavailable"))
	case "org.freedesktop.Secret.Error.NoSession", "org.freedesktop.DBus.Error.InvalidArgs",
		"org.freedesktop.DBus.Error.InvalidSignature", "org.freedesktop.DBus.Error.UnknownMethod",
		"org.freedesktop.DBus.Error.UnknownInterface":
		return fault.New(fault.AdapterRejected, op, errors.New("Secret Service rejected the protocol exchange"))
	default:
		return fault.New(fault.Unavailable, op, errors.New("Secret Service request failed"))
	}
}

func dbusErrorName(err error) string {
	var pointer *dbus.Error
	if errors.As(err, &pointer) && pointer != nil {
		return pointer.Name
	}
	var value dbus.Error
	if errors.As(err, &value) {
		return value.Name
	}
	return ""
}
