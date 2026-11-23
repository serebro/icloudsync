package icloud

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
)

// SRP-6a client compatible with Apple's idmsa.apple.com sign-in, which is
// what python "srp" (with rfc5054_enable + no_username_in_x, SHA256, NG_2048)
// produces for pyicloud/icloudpd.

const ng2048Hex = "AC6BDB41324A9A9BF166DE5E1389582FAF72B6651987EE07FC3192943DB56050A37329CBB4A099ED8193E0757767A13DD52312AB4B03310DCD7F48A9DA04FD50E8083969EDB767B0CF6095179A163AB3661A05FBD5FAAAE82918A9962F0B93B855F97993EC975EEAA80D740ADBF4FF747359D041D5C33EA71D281E446B14773BCA97B43A23FB801676BD207A436C6481F1D2B9078717461A5B9D32E688F87748544523B524B0D57D5EA77A2775D2ECFA032CFBDBF52FB3786160279004E57AE6AF874E7303CE53299CCC041C7BC308D82A5698F3A8D0C38271AE35F8E9DBFBB694B5C803D89F7AE435DE236D525F54759B65E372FCD68EF20FA7111F9E4AFF73"

var (
	srpN = mustHexInt(ng2048Hex)
	srpG = big.NewInt(2)
)

func mustHexInt(s string) *big.Int {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return new(big.Int).SetBytes(b)
}

func sha256Sum(parts ...[]byte) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// pad left-pads b with zeros to the byte length of N.
func pad(b []byte) []byte {
	n := len(srpN.Bytes())
	if len(b) >= n {
		return b
	}
	out := make([]byte, n)
	copy(out[n-len(b):], b)
	return out
}

type srpClient struct {
	username string
	a        *big.Int
	A        *big.Int
}

func newSRPClient(username string) (*srpClient, error) {
	buf := make([]byte, 256)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	buf[0] |= 0x80
	return newSRPClientWithA(username, buf), nil
}

func newSRPClientWithA(username string, aBytes []byte) *srpClient {
	a := new(big.Int).SetBytes(aBytes)
	return &srpClient{username: username, a: a, A: new(big.Int).Exp(srpG, a, srpN)}
}

func (c *srpClient) PublicA() []byte { return c.A.Bytes() }

// deriveSRPPassword turns the plain password into the SRP "password" Apple expects.
func deriveSRPPassword(password, protocol string, salt []byte, iterations int) ([]byte, error) {
	digest := sha256.Sum256([]byte(password))
	in := digest[:]
	switch protocol {
	case "s2k":
	case "s2k_fo":
		in = []byte(hex.EncodeToString(digest[:]))
	default:
		return nil, errors.New("unsupported SRP protocol: " + protocol)
	}
	return pbkdf2.Key(sha256.New, string(in), salt, iterations, 32)
}

// ProcessChallenge returns M1 and M2 (H_AMK) for the given salt, server B and derived password.
func (c *srpClient) ProcessChallenge(salt, bBytes, derivedPassword []byte) (m1, m2 []byte, err error) {
	N := srpN
	B := new(big.Int).SetBytes(bBytes)
	if new(big.Int).Mod(B, N).Sign() == 0 {
		return nil, nil, errors.New("srp: invalid server public key")
	}
	k := new(big.Int).SetBytes(sha256Sum(pad(N.Bytes()), pad(srpG.Bytes())))
	u := new(big.Int).SetBytes(sha256Sum(pad(c.A.Bytes()), pad(B.Bytes())))
	if u.Sign() == 0 {
		return nil, nil, errors.New("srp: invalid u")
	}
	inner := sha256Sum([]byte(":"), derivedPassword)
	x := new(big.Int).SetBytes(sha256Sum(salt, inner))
	v := new(big.Int).Exp(srpG, x, N)

	// S = (B - k*v) ^ (a + u*x) mod N
	base := new(big.Int).Sub(B, new(big.Int).Mul(k, v))
	base.Mod(base, N)
	exp := new(big.Int).Add(c.a, new(big.Int).Mul(u, x))
	S := new(big.Int).Exp(base, exp, N)
	K := sha256Sum(S.Bytes())

	hN := sha256Sum(N.Bytes())
	hg := sha256Sum(pad(srpG.Bytes()))
	xor := make([]byte, len(hN))
	for i := range hN {
		xor[i] = hN[i] ^ hg[i]
	}
	m1 = sha256Sum(xor, sha256Sum([]byte(c.username)), salt, c.A.Bytes(), B.Bytes(), K)
	m2 = sha256Sum(c.A.Bytes(), m1, K)
	return m1, m2, nil
}
