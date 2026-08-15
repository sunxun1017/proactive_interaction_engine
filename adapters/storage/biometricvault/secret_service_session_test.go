package biometricvault

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"reflect"
	"testing"

	"github.com/godbus/dbus/v5"

	"proactive-interaction-engine/internal/domain/fault"
)

func TestSecretServiceDHExchangeAndSecretEncryption(t *testing.T) {
	t.Parallel()
	if bits := secretServiceDHPrime().BitLen(); bits != 1024 {
		t.Fatalf("Second Oakley Group prime bits = %d, want 1024", bits)
	}
	clientPrivate, clientPublic, err := newDHKeyPair(bytes.NewReader(bytes.Repeat([]byte{0x31}, 256)))
	if err != nil {
		t.Fatal(err)
	}
	serverPrivate, serverPublic, err := newDHKeyPair(bytes.NewReader(bytes.Repeat([]byte{0x52}, 256)))
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := deriveDHSessionKey(clientPrivate, serverPublic)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := deriveDHSessionKey(serverPrivate, clientPublic)
	if err != nil {
		t.Fatal(err)
	}
	if clientKey != serverKey {
		t.Fatal("DH peers derived different AES keys")
	}
	session := dbus.ObjectPath("/org/freedesktop/secrets/session/test")
	plain := testKey(61)
	secret, err := encryptSecretValue(bytes.NewReader(bytes.Repeat([]byte{0xa4}, 32)), clientKey, session, plain[:])
	if err != nil {
		t.Fatal(err)
	}
	opened, err := decryptSecretValue(serverKey, session, secret)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, plain[:]) {
		t.Fatal("encrypted secret did not round trip")
	}
	if bytes.Contains(secret.Value, plain[:]) {
		t.Fatal("D-Bus Secret value contains plaintext master key")
	}
}

func TestSecretServiceRejectsInvalidDHAndCiphertext(t *testing.T) {
	t.Parallel()
	private := big.NewInt(11)
	prime := secretServiceDHPrime()
	for _, peer := range []*big.Int{big.NewInt(0), big.NewInt(1), new(big.Int).Sub(prime, big.NewInt(1)), prime} {
		if _, err := deriveDHSessionKey(private, peer.Bytes()); err == nil {
			t.Fatalf("deriveDHSessionKey(%s) error = nil", peer)
		}
	}
	if _, err := deriveDHSessionKey(private, make([]byte, 129)); err == nil {
		t.Fatal("deriveDHSessionKey(oversized peer) error = nil")
	}
	key := [16]byte{1, 2, 3}
	session := dbus.ObjectPath("/session/test")
	secret, err := encryptSecretValue(bytes.NewReader(bytes.Repeat([]byte{7}, 16)), key, session, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*secretValue)
	}{
		{name: "wrong session", mutate: func(value *secretValue) { value.Session = "/session/other" }},
		{name: "wrong content type", mutate: func(value *secretValue) { value.ContentType = "text/plain" }},
		{name: "wrong iv", mutate: func(value *secretValue) { value.Parameters = []byte{1} }},
		{name: "wrong block size", mutate: func(value *secretValue) { value.Value = []byte{1} }},
		{name: "wrong padding", mutate: func(value *secretValue) { value.Value[len(value.Value)-1] ^= 0xff }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copy := secretValue{
				Session: secret.Session, Parameters: bytes.Clone(secret.Parameters),
				Value: bytes.Clone(secret.Value), ContentType: secret.ContentType,
			}
			test.mutate(&copy)
			if _, err := decryptSecretValue(key, session, copy); err == nil {
				t.Fatal("decryptSecretValue() error = nil")
			}
		})
	}
}

func TestDBusSecretServiceOpenSessionUsesEncryptedDHOnly(t *testing.T) {
	t.Parallel()
	serverPrivate := big.NewInt(19)
	serverPublic := new(big.Int).Exp(big.NewInt(2), serverPrivate, secretServiceDHPrime()).Bytes()
	connection := &fakeSecretServiceConnection{}
	connection.handle = func(_ dbus.ObjectPath, method string, args ...any) *dbus.Call {
		if method != secretServiceInterface+".OpenSession" {
			return dbusCall(nil, errors.New("unexpected method"))
		}
		if got := args[0]; got != secretServiceAlgorithm || got == "plain" {
			t.Fatalf("OpenSession algorithm = %v, want encrypted DH", got)
		}
		input, ok := args[1].(dbus.Variant)
		if !ok {
			t.Fatalf("OpenSession input type = %T", args[1])
		}
		if public, ok := input.Value().([]byte); !ok || len(public) == 0 {
			t.Fatalf("OpenSession public input = %#v", input.Value())
		}
		return dbusCall([]any{dbus.MakeVariant(serverPublic), dbus.ObjectPath("/session/dh")}, nil)
	}
	store := &dbusSecretServiceStore{connection: connection, random: bytes.NewReader(bytes.Repeat([]byte{0x22}, 256))}
	if err := store.openSession(context.Background()); err != nil {
		t.Fatalf("openSession() error = %v", err)
	}
	if store.session != "/session/dh" || store.aesKey == [16]byte{} {
		t.Fatalf("openSession() did not retain valid encrypted session state")
	}
}

func TestDBusSecretServiceCreateRejectsPromptAndDismissesIt(t *testing.T) {
	t.Parallel()
	connection := &fakeSecretServiceConnection{}
	var methods []string
	connection.handle = func(path dbus.ObjectPath, method string, args ...any) *dbus.Call {
		methods = append(methods, method)
		switch method {
		case secretServiceInterface + ".ReadAlias":
			return dbusCall([]any{dbus.ObjectPath("/collection/default")}, nil)
		case propertiesInterface + ".Get":
			return dbusCall([]any{dbus.MakeVariant(false)}, nil)
		case secretCollectionInterface + ".CreateItem":
			properties, ok := args[0].(map[string]dbus.Variant)
			if !ok {
				t.Fatalf("CreateItem properties type = %T", args[0])
			}
			attributes, ok := properties[secretItemInterface+".Attributes"].Value().(map[string]string)
			if !ok || !reflect.DeepEqual(attributes, cloneAttributes()) {
				t.Fatalf("CreateItem attributes = %#v", attributes)
			}
			if replace, ok := args[2].(bool); !ok || replace {
				t.Fatalf("CreateItem replace = %#v, want false", args[2])
			}
			secret := args[1].(secretValue)
			plainKey := testKey(9)
			if secret.ContentType != secretServiceContentType || bytes.Equal(secret.Value, plainKey[:]) {
				t.Fatal("CreateItem secret was not encrypted")
			}
			return dbusCall([]any{dbus.ObjectPath("/item/new"), dbus.ObjectPath("/prompt/required")}, nil)
		case secretPromptInterface + ".Dismiss":
			if path != "/prompt/required" {
				t.Fatalf("Dismiss path = %s", path)
			}
			return dbusCall(nil, nil)
		default:
			return dbusCall(nil, errors.New("unexpected method"))
		}
	}
	store := &dbusSecretServiceStore{
		connection: connection, random: bytes.NewReader(bytes.Repeat([]byte{0x44}, 32)),
		session: "/session/test", aesKey: [16]byte{7, 8, 9},
	}
	if err := store.Create(context.Background(), testKey(9)); !fault.IsCode(err, fault.PermissionDenied) {
		t.Fatalf("Create() error = %v, want PermissionDenied", err)
	}
	want := []string{
		secretServiceInterface + ".ReadAlias", propertiesInterface + ".Get",
		secretCollectionInterface + ".CreateItem", secretPromptInterface + ".Dismiss",
	}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
}

func TestDBusSecretServiceLookupRejectsLockedAndAmbiguousItems(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		unlocked []dbus.ObjectPath
		locked   []dbus.ObjectPath
		want     fault.Code
	}{
		{name: "locked", locked: []dbus.ObjectPath{"/item/locked"}, want: fault.PermissionDenied},
		{name: "ambiguous", unlocked: []dbus.ObjectPath{"/item/one", "/item/two"}, want: fault.AdapterRejected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			connection := &fakeSecretServiceConnection{handle: func(_ dbus.ObjectPath, method string, args ...any) *dbus.Call {
				calls++
				if method != secretServiceInterface+".SearchItems" || !reflect.DeepEqual(args[0], cloneAttributes()) {
					t.Fatalf("SearchItems call = %s %#v", method, args)
				}
				return dbusCall([]any{test.unlocked, test.locked}, nil)
			}}
			store := &dbusSecretServiceStore{connection: connection, session: "/session/test"}
			if _, _, err := store.Lookup(context.Background()); !fault.IsCode(err, test.want) {
				t.Fatalf("Lookup() error = %v, want %s", err, test.want)
			}
			if calls != 1 {
				t.Fatalf("D-Bus calls = %d, want 1", calls)
			}
		})
	}
}

func TestSecretServiceFaultMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want fault.Code
	}{
		{name: "locked", err: dbus.NewError("org.freedesktop.Secret.Error.IsLocked", nil), want: fault.PermissionDenied},
		{name: "not supported", err: dbus.NewError("org.freedesktop.DBus.Error.NotSupported", nil), want: fault.Unavailable},
		{name: "invalid args", err: dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", nil), want: fault.AdapterRejected},
		{name: "deadline", err: context.DeadlineExceeded, want: fault.DeadlineExceeded},
		{name: "canceled", err: context.Canceled, want: fault.Unavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifySecretServiceFault("test", test.err); !fault.IsCode(got, test.want) {
				t.Fatalf("classifySecretServiceFault() = %v, want %s", got, test.want)
			}
		})
	}
}

func TestSecretServiceAttributesContainOnlyFixedNonPIIValues(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"application": "proactive-interaction-engine",
		"purpose":     "biometric-vault-master-key",
		"version":     "1",
		"xdg:schema":  "org.proactiveinteractionengine.BiometricMasterKey",
	}
	if got := cloneAttributes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("attributes = %#v, want %#v", got, want)
	}
}

type fakeSecretServiceConnection struct {
	handle func(dbus.ObjectPath, string, ...any) *dbus.Call
	closed bool
}

func (c *fakeSecretServiceConnection) Object(_ string, path dbus.ObjectPath) dbus.BusObject {
	return fakeSecretServiceObject{path: path, handle: c.handle}
}

func (c *fakeSecretServiceConnection) Close() error {
	c.closed = true
	return nil
}

type fakeSecretServiceObject struct {
	dbus.BusObject
	path   dbus.ObjectPath
	handle func(dbus.ObjectPath, string, ...any) *dbus.Call
}

func (o fakeSecretServiceObject) CallWithContext(_ context.Context, method string, _ dbus.Flags, args ...any) *dbus.Call {
	return o.handle(o.path, method, args...)
}

func dbusCall(body []any, err error) *dbus.Call {
	return &dbus.Call{Body: body, Err: err}
}
