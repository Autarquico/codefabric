// Package pki is the Controller's identity: the CA that anchors trust (its
// key is the Controller's identity, §16.6), the certificates it issues and the
// single-use tokens that bootstrap them. Everything is ECDSA P-256 (ADR 0002 D2).
package pki
