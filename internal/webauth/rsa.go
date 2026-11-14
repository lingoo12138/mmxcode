package webauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
)

// publicKeyPEM is the key the web page encrypts the password with.
//
// The page hard-codes it (js/204, module 7160) and encrypts with
// RSA-1024 and PKCS#1 v1.5, which is what JSEncrypt does. The cipher
// text is 128 bytes, and every time it is different: the padding is
// random.
const publicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDF5ndG2/UB4L5tbvQaNLHSoBTW
DKbrNBuOmUIP23eCmC2ELMx3kppEikxTp5cV8NxUZl6ii+KLwKugioAXApzypHXb
gXbq13kTKA7OCA1xtAoMdH9cltjBiFAUJlgmVjr0MuJCknhVAjWLjCVRHege+Atl
gkUBUeGa9O+cWcPEwQIDAQAB
-----END PUBLIC KEY-----`

// encryptPassword encrypts a password the way the web page does.
//
// The result is base64 of the RSA cipher text, which is what the site
// expects in the "authToken" field.
//
// The padding of PKCS#1 v1.5 is random, so the same password gives a
// different cipher text every time. The cipher text is not a password
// hash and must never be stored as one.
func encryptPassword(password string) (string, error) {
	block, _ := pem.Decode([]byte(publicKeyPEM))
	if block == nil {
		return "", errors.New("the site's key is not a key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", err
	}
	key, ok := pub.(*rsa.PublicKey)
	if !ok {
		return "", errors.New("the site's key is not an RSA key")
	}
	cipher, err := rsa.EncryptPKCS1v15(rand.Reader, key, []byte(password))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(cipher), nil
}
