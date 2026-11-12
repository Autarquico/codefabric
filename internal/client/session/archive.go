package session

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// maxManifestBytes bounds the manifest an archive starts with.
const maxManifestBytes = 1 << 20

// canonicalID is the form of the id of a session of either harness: no id of an archive
// reaches a path unless it has it.
var canonicalID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// readManifest is the manifest an archive of a session starts with: which harness wrote
// it and under which id.
func readManifest(archive []byte) (harness.SessionManifest, error) {
	var m harness.SessionManifest
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return m, fmt.Errorf("session: the archive is not readable: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil || h.Name != manifestName {
		return m, errors.New("session: the archive does not start with its manifest")
	}
	raw, err := io.ReadAll(io.LimitReader(tr, maxManifestBytes))
	if err != nil {
		return m, fmt.Errorf("session: the archive is not readable: %w", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Adapter == "" || !canonicalID.MatchString(m.SessionID) {
		return harness.SessionManifest{}, errors.New("session: the manifest of the archive is not valid")
	}
	return m, nil
}
