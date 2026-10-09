package aptpackage

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
)

type DestinationKind string

const (
	DestinationLocal DestinationKind = "local"
	DestinationSSH   DestinationKind = "ssh-sftp"
)

type SSHPathMode string

const (
	SSHPathAbsolute     SSHPathMode = "absolute"
	SSHPathHomeRelative SSHPathMode = "home-relative"
)

type RepositoryDestination struct {
	Kind          DestinationKind
	LocalPath     string
	Host          string
	User          string
	Port          int
	PortExplicit  bool
	SSHConfigPath string
	RemotePath    string
	PathMode      SSHPathMode
}

var destinationUserPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ParseRepositoryDestination distinguishes local paths, SCP-style SSH paths,
// and explicit SSH URLs. Relative SCP paths are anchored to the SSH user's home;
// URL paths are absolute except for the explicit /~/path home-relative form.
func ParseRepositoryDestination(value string) (RepositoryDestination, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
		return RepositoryDestination{}, fmt.Errorf("output destination is empty or contains invalid whitespace")
	}
	if strings.Contains(value, "://") {
		return parseSSHOutputURL(value)
	}
	if isLocalDrivePath(value) || strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../") || value == "." || value == ".." || pathIsAbsoluteLocal(value) {
		return RepositoryDestination{Kind: DestinationLocal, LocalPath: value}, nil
	}
	// Existing local names containing colons remain local. Use ./name:part to
	// disambiguate a new local filename from the SCP destination grammar.
	if _, err := os.Lstat(value); err == nil {
		return RepositoryDestination{Kind: DestinationLocal, LocalPath: value}, nil
	}
	if destination, matched, err := parseSCPDestination(value); matched {
		return destination, err
	}
	return RepositoryDestination{Kind: DestinationLocal, LocalPath: value}, nil
}

func parseSSHOutputURL(value string) (RepositoryDestination, error) {
	u, err := url.Parse(value)
	if err != nil {
		return RepositoryDestination{}, fmt.Errorf("invalid SSH output URL: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "ssh") || u.Opaque != "" || u.Host == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(value, "#") {
		return RepositoryDestination{}, fmt.Errorf("output URL must be an SSH URL without query or fragment")
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword || !validDestinationUser(u.User.Username()) {
			return RepositoryDestination{}, fmt.Errorf("SSH output URL has an invalid username or embedded password")
		}
	}
	host := u.Hostname()
	if !validSSHHost(host) {
		return RepositoryDestination{}, fmt.Errorf("SSH output URL has an invalid host")
	}
	port := 22
	portExplicit := false
	if rawPort := u.Port(); rawPort != "" {
		portExplicit = true
		port, err = strconv.Atoi(rawPort)
		if err != nil || port < 1 || port > 65535 {
			return RepositoryDestination{}, fmt.Errorf("SSH output URL has an invalid port")
		}
	}
	if strings.Contains(strings.ToLower(u.RawPath), "%2f") || strings.Contains(strings.ToLower(u.RawPath), "%5c") {
		return RepositoryDestination{}, fmt.Errorf("SSH output URL must not percent-encode path separators")
	}
	remotePath := u.Path
	if remotePath == "" || !strings.HasPrefix(remotePath, "/") || strings.ContainsAny(remotePath, "\\\x00\r\n") {
		return RepositoryDestination{}, fmt.Errorf("SSH output URL requires a valid absolute path")
	}
	mode := SSHPathAbsolute
	if strings.HasPrefix(remotePath, "/~/") {
		mode = SSHPathHomeRelative
		remotePath = strings.TrimPrefix(remotePath, "/~/")
	} else {
		remotePath = strings.TrimPrefix(remotePath, "/")
	}
	clean, err := cleanRemotePath(remotePath)
	if err != nil {
		return RepositoryDestination{}, err
	}
	return RepositoryDestination{
		Kind: DestinationSSH, Host: host, User: userFromURL(u), Port: port, PortExplicit: portExplicit,
		RemotePath: clean, PathMode: mode,
	}, nil
}

func userFromURL(u *url.URL) string {
	if u.User == nil {
		return ""
	}
	return u.User.Username()
}

func parseSCPDestination(value string) (RepositoryDestination, bool, error) {
	separator := -1
	hostPart := ""
	bracketStart := -1
	if strings.HasPrefix(value, "[") {
		bracketStart = 0
	} else if at := strings.Index(value, "@["); at >= 0 {
		bracketStart = at + 1
	}
	if bracketStart >= 0 {
		closeBracket := strings.IndexByte(value[bracketStart:], ']')
		if closeBracket < 0 {
			return RepositoryDestination{}, true, fmt.Errorf("invalid bracketed SSH destination; expected [host]:path")
		}
		closeBracket += bracketStart
		if closeBracket+1 >= len(value) || value[closeBracket+1] != ':' {
			return RepositoryDestination{}, true, fmt.Errorf("invalid bracketed SSH destination; expected [host]:path")
		}
		separator = closeBracket + 1
		hostPart = value[:closeBracket+1]
	} else {
		separator = strings.IndexByte(value, ':')
		if separator <= 0 || strings.Contains(value[:separator], "/") || strings.Contains(value[:separator], "\\") {
			return RepositoryDestination{}, false, nil
		}
		hostPart = value[:separator]
	}
	if separator < 0 {
		return RepositoryDestination{}, false, nil
	}
	pathPart := value[separator+1:]
	if pathPart == "" {
		return RepositoryDestination{}, true, fmt.Errorf("SSH shorthand requires a non-empty remote path")
	}
	user, host, err := parseSSHUserHost(hostPart)
	if err != nil {
		return RepositoryDestination{}, true, fmt.Errorf("invalid SSH shorthand destination: %w", err)
	}
	mode := SSHPathHomeRelative
	if strings.HasPrefix(pathPart, "/") {
		mode = SSHPathAbsolute
		pathPart = strings.TrimPrefix(pathPart, "/")
	} else if strings.HasPrefix(pathPart, "~/") {
		pathPart = strings.TrimPrefix(pathPart, "~/")
	}
	clean, err := cleanRemotePath(pathPart)
	if err != nil {
		return RepositoryDestination{}, true, err
	}
	return RepositoryDestination{Kind: DestinationSSH, Host: host, User: user, Port: 22, RemotePath: clean, PathMode: mode}, true, nil
}

func parseSSHUserHost(value string) (user, host string, err error) {
	if strings.Count(value, "@") > 1 {
		return "", "", fmt.Errorf("invalid user@host syntax")
	}
	if at := strings.IndexByte(value, '@'); at >= 0 {
		user, value = value[:at], value[at+1:]
		if !validDestinationUser(user) {
			return "", "", fmt.Errorf("invalid SSH username")
		}
	}
	if strings.HasPrefix(value, "[") {
		closeBracket := strings.IndexByte(value, ']')
		if closeBracket < 0 || closeBracket != len(value)-1 {
			return "", "", fmt.Errorf("invalid bracketed host")
		}
		host = value[1:closeBracket]
		if net.ParseIP(host) == nil || !strings.Contains(host, ":") {
			return "", "", fmt.Errorf("invalid IPv6 host")
		}
		return user, host, nil
	}
	host = value
	if !validSSHHost(host) {
		return "", "", fmt.Errorf("invalid SSH host")
	}
	return user, host, nil
}

func validDestinationUser(value string) bool {
	return value != "" && destinationUserPattern.MatchString(value)
}

func validSSHHost(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") || strings.ContainsAny(value, "\x00\r\n/\\@[]") {
		return false
	}
	if strings.Contains(value, ":") {
		return net.ParseIP(value) != nil
	}
	for _, character := range value {
		if !(character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '.' || character == '-' || character == '_') {
			return false
		}
	}
	return value != ""
}

func cleanRemotePath(value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\\\x00\r\n") {
		return "", fmt.Errorf("SSH output path must not be empty or contain control characters")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." || segment == "." || segment == "" {
			return "", fmt.Errorf("SSH output path must not contain traversal or ambiguous path segments")
		}
	}
	clean := path.Clean(value)
	if clean == "." || clean != value {
		return "", fmt.Errorf("SSH output path is not canonical")
	}
	return clean, nil
}

func isLocalDrivePath(value string) bool {
	return len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' && (value[2] == '/' || value[2] == '\\')
}

func pathIsAbsoluteLocal(value string) bool {
	return strings.HasPrefix(value, "/")
}

func isAbsoluteRemoteDestination(destination RepositoryDestination) bool {
	return destination.PathMode == SSHPathAbsolute
}
