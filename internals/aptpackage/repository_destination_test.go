package aptpackage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepositoryDestinationParsing(t *testing.T) {
	cases := []struct {
		name         string
		value        string
		kind         DestinationKind
		host         string
		user         string
		port         int
		portExplicit bool
		remotePath   string
		pathMode     SSHPathMode
		wantErr      bool
	}{
		{name: "relative local", value: "./repo", kind: DestinationLocal},
		{name: "absolute local", value: "/srv/repos/example", kind: DestinationLocal},
		{name: "scp home path", value: "nas:repo", kind: DestinationSSH, host: "nas", port: 22, remotePath: "repo", pathMode: SSHPathHomeRelative},
		{name: "scp user path", value: "eugen@nas:repositories/example", kind: DestinationSSH, host: "nas", user: "eugen", port: 22, remotePath: "repositories/example", pathMode: SSHPathHomeRelative},
		{name: "scp absolute path", value: "nas:/srv/repos/example", kind: DestinationSSH, host: "nas", port: 22, remotePath: "srv/repos/example", pathMode: SSHPathAbsolute},
		{name: "scp tilde path", value: "nas:~/repositories/example", kind: DestinationSSH, host: "nas", port: 22, remotePath: "repositories/example", pathMode: SSHPathHomeRelative},
		{name: "ssh absolute url", value: "ssh://nas/repo", kind: DestinationSSH, host: "nas", port: 22, remotePath: "repo", pathMode: SSHPathAbsolute},
		{name: "ssh home url", value: "ssh://eugen@nas/~/repo", kind: DestinationSSH, host: "nas", user: "eugen", port: 22, remotePath: "repo", pathMode: SSHPathHomeRelative},
		{name: "encoded space", value: "ssh://nas/apt/repo%20name", kind: DestinationSSH, host: "nas", port: 22, remotePath: "apt/repo name", pathMode: SSHPathAbsolute},
		{name: "ssh port url", value: "ssh://eugen@nas:2222/srv/repos/example", kind: DestinationSSH, host: "nas", user: "eugen", port: 2222, portExplicit: true, remotePath: "srv/repos/example", pathMode: SSHPathAbsolute},
		{name: "IPv6 shorthand", value: "eugen@[2001:db8::1]:repo", kind: DestinationSSH, host: "2001:db8::1", user: "eugen", port: 22, remotePath: "repo", pathMode: SSHPathHomeRelative},
		{name: "IPv6 URL", value: "ssh://eugen@[2001:db8::1]:2222/repo", kind: DestinationSSH, host: "2001:db8::1", user: "eugen", port: 2222, portExplicit: true, remotePath: "repo", pathMode: SSHPathAbsolute},
		{name: "invalid URL port syntax", value: "ssh://nas:repo", wantErr: true},
		{name: "bad port", value: "ssh://nas:70000/repo", wantErr: true},
		{name: "password URL", value: "ssh://user:secret@nas/repo", wantErr: true},
		{name: "unsupported scheme", value: "sftp://nas/repo", wantErr: true},
		{name: "query", value: "ssh://nas/repo?x=1", wantErr: true},
		{name: "empty query", value: "ssh://nas/repo?", wantErr: true},
		{name: "empty fragment", value: "ssh://nas/repo#", wantErr: true},
		{name: "traversal shorthand", value: "nas:../repo", wantErr: true},
		{name: "traversal URL", value: "ssh://nas/a/../repo", wantErr: true},
		{name: "encoded slash", value: "ssh://nas/a%2Fb", wantErr: true},
		{name: "empty SSH path", value: "nas:", wantErr: true},
		{name: "invalid SSH shorthand host", value: "bad host:repo", wantErr: true},
		{name: "option-like SSH shorthand host", value: "-option:repo", wantErr: true},
		{name: "bad IPv6", value: "user@[not-ip]:repo", wantErr: true},
		{name: "surrounding whitespace", value: " nas:repo", wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseRepositoryDestination(test.value)
			if (err != nil) != test.wantErr {
				t.Fatalf("ParseRepositoryDestination(%q) = %+v, error=%v, wantErr=%v", test.value, got, err, test.wantErr)
			}
			if err != nil {
				return
			}
			if got.Kind != test.kind || got.Host != test.host || got.User != test.user || got.Port != test.port || got.PortExplicit != test.portExplicit || got.RemotePath != test.remotePath || got.PathMode != test.pathMode {
				t.Fatalf("ParseRepositoryDestination(%q) = %+v", test.value, got)
			}
		})
	}
}

func TestRepositoryDestinationColonLocalCompatibility(t *testing.T) {
	root := t.TempDir()
	localName := filepath.Join(root, "nas:repo")
	if err := os.Mkdir(localName, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ParseRepositoryDestination(localName)
	if err != nil || got.Kind != DestinationLocal || got.LocalPath != localName {
		t.Fatalf("existing colon-containing local path parsed as %+v, %v", got, err)
	}
	got, err = ParseRepositoryDestination("./nas:repo")
	if err != nil || got.Kind != DestinationLocal || got.LocalPath != "./nas:repo" {
		t.Fatalf("explicit relative colon path parsed as %+v, %v", got, err)
	}
}
