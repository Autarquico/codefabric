package client

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"

	"github.com/Autarquico/codefabric/internal/domain"
)

func TestTransportError_OnlyCertificateAlertsAreAuthenticationFailures(t *testing.T) {
	const (
		internalError    alertStub = 80
		protocolVersion  alertStub = 70
		handshakeFailure alertStub = 40
	)
	table := []struct {
		alert alertStub
		want  domain.Code
	}{
		{alertBadCertificate, domain.CodeUnauthenticated},
		{alertCertificateRevoked, domain.CodeUnauthenticated},
		{alertCertificateExpired, domain.CodeUnauthenticated},
		{alertUnknownCA, domain.CodeUnauthenticated},
		{alertCertificateRequired, domain.CodeUnauthenticated},
		{internalError, domain.CodeControllerUnreachable},
		{protocolVersion, domain.CodeControllerUnreachable},
		{handshakeFailure, domain.CodeControllerUnreachable},
	}
	c := &Client{controller: "host:1"}
	for _, tc := range table {
		t.Run(strconv.Itoa(int(tc.alert)), func(t *testing.T) {
			err := c.transportError(context.Background(), &net.OpError{Op: "remote error", Err: tc.alert})
			if !isCode(err, tc.want) {
				t.Fatalf("alert %d: got %v, want %s", uint8(tc.alert), err, tc.want)
			}
			var de *domain.Error
			if !errors.As(err, &de) || de.Details["cause"] == nil {
				t.Fatalf("alert %d: the local cause is missing for --debug: %#v", uint8(tc.alert), err)
			}
		})
	}
}

// alertStub has the shape of crypto/tls's unexported alert type: a uint8.
type alertStub uint8

func (a alertStub) Error() string { return "tls alert " + strconv.Itoa(int(a)) }

func isCode(err error, code domain.Code) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == code
}
