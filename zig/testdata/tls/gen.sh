#!/bin/sh
# Regenerates the TLS test certificates (run once; the PEM files are committed).
# Needs OpenSSL 3.4+ (the x509 -not_before/-not_after options); on macOS use
# the Homebrew one: OPENSSL=/opt/homebrew/opt/openssl@3/bin/openssl sh gen.sh
# The CA key is thrown away on purpose: nothing may sign with it later.
set -eu
O=${OPENSSL:-openssl}
cd "$(dirname "$0")"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

$O ecparam -name prime256v1 -genkey -noout -out "$tmp/ca.key"
$O req -x509 -new -key "$tmp/ca.key" -sha256 -days 36500 -subj "/CN=bitnews test CA" \
	-addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" -out ca.pem

# leaf NAME CN SAN NOT_BEFORE NOT_AFTER
leaf() {
	$O ecparam -name prime256v1 -genkey -noout | $O pkcs8 -topk8 -nocrypt -out "$1.key"
	$O req -new -key "$1.key" -subj "/CN=$2" -out "$tmp/$1.csr"
	printf 'basicConstraints=CA:FALSE\nkeyUsage=digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=%s\n' "$3" >"$tmp/$1.ext"
	$O x509 -req -in "$tmp/$1.csr" -CA ca.pem -CAkey "$tmp/ca.key" -CAcreateserial -CAserial "$tmp/ca.srl" \
		-sha256 -not_before "$4" -not_after "$5" -extfile "$tmp/$1.ext" -out "$1.pem"
}

leaf server localhost "DNS:localhost,IP:127.0.0.1,IP:::1" 20250101000000Z 21250101000000Z
leaf expired localhost "DNS:localhost,IP:127.0.0.1,IP:::1" 20200101000000Z 20200102000000Z
leaf wronghost other.example.test "DNS:other.example.test" 20250101000000Z 21250101000000Z

# A second, unrelated CA with its own server certificate (unknown authority).
$O ecparam -name prime256v1 -genkey -noout -out "$tmp/ca2.key"
$O req -x509 -new -key "$tmp/ca2.key" -sha256 -days 36500 -subj "/CN=bitnews other test CA" \
	-addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" -out other_ca.pem
$O ecparam -name prime256v1 -genkey -noout | $O pkcs8 -topk8 -nocrypt -out untrusted.key
$O req -new -key untrusted.key -subj "/CN=localhost" -out "$tmp/u.csr"
printf 'basicConstraints=CA:FALSE\nkeyUsage=digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:localhost,IP:127.0.0.1\n' >"$tmp/u.ext"
$O x509 -req -in "$tmp/u.csr" -CA other_ca.pem -CAkey "$tmp/ca2.key" -CAcreateserial -CAserial "$tmp/ca2.srl" \
	-sha256 -not_before 20250101000000Z -not_after 21250101000000Z -extfile "$tmp/u.ext" -out untrusted.pem
