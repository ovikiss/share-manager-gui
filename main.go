package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	smbConf        = getenv("SMB_CONF_PATH", "")
	exports        = getenv("NFS_EXPORTS_PATH", "")
	exportsD       = getenv("NFS_EXPORTS_DIR", "")
	backups        = getenv("BACKUP_DIR", "")
	backupDisplay  = getenv("BACKUP_DISPLAY_DIR", "")
	sambaDB        = getenv("SAMBA_DB_PATH", "")
	fileUID        = getenvInt("FILE_UID", -1)
	fileGID        = getenvInt("FILE_GID", -1)
	browseRoot     = getenv("BROWSE_ROOT", "")
	recycleDir     = getenv("RECYCLE_DIR", "")
	nfsManagerFile = getenv("NFS_MANAGER_FILENAME", "")
	uiFile         = getenv("UI_FILE", "")
	staticDir      = getenv("STATIC_DIR", "")
	uiSettingsFile = getenv("UI_SETTINGS_PATH", "")
	maxRecycle     = getenvInt("MAX_RECYCLE_ENTRIES", 0)
	minUserUID     = getenvInt("MIN_USER_UID", 0)
	nsenterPID     = getenvInt("NSENTER_PID", -1)
	smbService     = getenv("SMB_SERVICE", "")
	mu             sync.Mutex
)

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	value := getenv(key, "")
	if value == "" {
		return fallback
	}
	var parsed int
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

type section struct {
	name  string
	lines []string
}
type sambaShare struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	ReadOnly   bool   `json:"read_only"`
	GuestOK    bool   `json:"guest_ok"`
	ValidUsers string `json:"valid_users"`
	WriteList  string `json:"write_list"`
	ReadList   string `json:"read_list"`
	Recycle    bool   `json:"recycle"`
}
type nfsExport struct {
	Path    string `json:"path"`
	Clients string `json:"clients"`
	Client  string `json:"client"`
	Options string `json:"options"`
	File    string `json:"file"`
}
type recycleEntry struct {
	Share string `json:"share"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
}
type browseEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func hostCommand(args ...string) *exec.Cmd {
	return exec.Command("nsenter", append([]string{"-t", fmt.Sprint(nsenterPID), "-m", "-u", "-i", "-n", "-p", "--"}, args...)...)
}
func runHost(args ...string) (string, string, error) {
	cmd := hostCommand(args...)
	out, err := cmd.Output()
	if err == nil {
		return string(out), "", nil
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return "", string(exit.Stderr), err
	}
	return "", err.Error(), err
}
func runHostInput(input string, args ...string) (string, string, error) {
	cmd := hostCommand(args...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	if err == nil {
		return string(out), "", nil
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return "", string(exit.Stderr), err
	}
	return "", err.Error(), err
}
func read(path string) string { data, _ := os.ReadFile(path); return string(data) }
func mustJSON(value any) string {
	data, _ := json.MarshalIndent(value, "", "  ")
	return string(data) + "\n"
}
func atomicWrite(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	mode := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".share-manager-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(mode); err == nil {
		_, err = tmp.WriteString(content)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
func backup() (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	dir := filepath.Join(backups, stamp)
	if err := os.MkdirAll(dir, 0770); err != nil {
		return "", err
	}
	if err := os.Chown(dir, fileUID, fileGID); err != nil {
		return "", err
	}
	copyFile := func(src, dst string) error {
		data, err := os.ReadFile(src)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0600); err != nil {
			return err
		}
		return os.Chown(dst, fileUID, fileGID)
	}
	if err := copyFile(smbConf, filepath.Join(dir, "smb.conf")); err != nil {
		return "", err
	}
	if err := copyFile(exports, filepath.Join(dir, "exports")); err != nil {
		return "", err
	}
	if err := copyFile(sambaDB, filepath.Join(dir, "passdb.tdb")); err != nil {
		return "", err
	}
	if entries, err := os.ReadDir(exportsD); err == nil {
		exportsBackupDir := filepath.Join(dir, "exports.d")
		if err = os.MkdirAll(exportsBackupDir, 0770); err != nil {
			return "", err
		}
		if err = os.Chown(exportsBackupDir, fileUID, fileGID); err != nil {
			return "", err
		}
		if err = os.Chmod(exportsBackupDir, 0770); err != nil {
			return "", err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				if err = copyFile(filepath.Join(exportsD, entry.Name()), filepath.Join(dir, "exports.d", entry.Name())); err != nil {
					return "", err
				}
			}
		}
	}
	return filepath.Join(backupDisplay, stamp), nil
}
func splitSections(text string) []section {
	var sections []section
	current := -1
	for _, line := range strings.SplitAfter(text, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]") {
			sections = append(sections, section{name: strings.TrimSpace(trim[1 : len(trim)-1])})
			current = len(sections) - 1
		} else {
			if current < 0 {
				sections = append(sections, section{})
				current = 0
			}
			sections[current].lines = append(sections[current].lines, line)
		}
	}
	if len(sections) == 0 {
		sections = append(sections, section{})
	}
	return sections
}
func options(s section) map[string]string {
	result := map[string]string{}
	for _, line := range s.lines {
		value := strings.TrimSpace(line)
		if value == "" || strings.HasPrefix(value, "#") || strings.HasPrefix(value, ";") {
			continue
		}
		parts := strings.SplitN(value, "=", 2)
		if len(parts) == 2 {
			result[strings.ToLower(strings.TrimSpace(parts[0]))] = strings.TrimSpace(parts[1])
		}
	}
	return result
}
func render(sections []section) string {
	var b strings.Builder
	for _, s := range sections {
		if s.name != "" {
			b.WriteString("[" + s.name + "]\n")
		}
		for _, line := range s.lines {
			b.WriteString(line)
		}
		if s.name != "" {
			b.WriteString("\n")
		}
	}
	return b.String()
}
func state() ([]sambaShare, []nfsExport, []recycleEntry) {
	shares := make([]sambaShare, 0)
	for _, s := range splitSections(read(smbConf)) {
		if s.name == "" || strings.EqualFold(s.name, "global") {
			continue
		}
		o := options(s)
		ro := strings.EqualFold(o["read only"], "yes") || strings.EqualFold(o["writable"], "no")
		vo := strings.Fields(o["vfs objects"])
		recycle := false
		for _, value := range vo {
			if value == "recycle" {
				recycle = true
			}
		}
		shares = append(shares, sambaShare{Name: s.name, Path: o["path"], ReadOnly: ro, GuestOK: strings.EqualFold(o["guest ok"], "yes"), ValidUsers: o["valid users"], WriteList: o["write list"], ReadList: o["read list"], Recycle: recycle})
	}
	nfs := make([]nfsExport, 0)
	files := []string{exports}
	if entries, err := os.ReadDir(exportsD); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".exports") {
				files = append(files, filepath.Join(exportsD, e.Name()))
			}
		}
	}
	for _, file := range files {
		for _, line := range strings.Split(read(file), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.Fields(line)
			if len(parts) > 1 {
				clients := strings.Join(parts[1:], " ")
				client, options := splitNFSClient(clients)
				nfs = append(nfs, nfsExport{Path: parts[0], Clients: clients, Client: client, Options: options, File: file})
			}
		}
	}
	recycled := make([]recycleEntry, 0)
	for _, share := range shares {
		if !share.Recycle {
			continue
		}
		root := filepath.Join(share.Path, recycleDir)
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.Mode().IsRegular() && len(recycled) < maxRecycle {
				rel, _ := filepath.Rel(root, path)
				recycled = append(recycled, recycleEntry{Share: share.Name, Path: rel, Size: info.Size()})
			}
			return nil
		})
	}
	return shares, nfs, recycled
}

func splitNFSClient(value string) (string, string) {
	value = strings.TrimSpace(value)
	if i := strings.IndexByte(value, '('); i > 0 {
		return strings.TrimSpace(value[:i]), strings.TrimSpace(value[i:])
	}
	parts := strings.Fields(value)
	if len(parts) == 0 {
		return "", ""
	}
	return parts[0], strings.TrimSpace(strings.Join(parts[1:], " "))
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`)

func editShare(input sambaShare) (string, error) {
	if !namePattern.MatchString(input.Name) || !strings.HasPrefix(input.Path, "/") {
		return "", errors.New("invalid share name or absolute path required")
	}
	sections := splitSections(read(smbConf))
	lines := []string{fmt.Sprintf("   path = %s\n", input.Path), "   browseable = yes\n", fmt.Sprintf("   read only = %s\n", yesNo(input.ReadOnly)), fmt.Sprintf("   guest ok = %s\n", yesNo(input.GuestOK))}
	for _, item := range []struct{ k, v string }{{"valid users", input.ValidUsers}, {"write list", input.WriteList}, {"read list", input.ReadList}} {
		if strings.TrimSpace(item.v) != "" {
			lines = append(lines, fmt.Sprintf("   %s = %s\n", item.k, strings.TrimSpace(item.v)))
		}
	}
	if input.Recycle {
		lines = append(lines, "   vfs objects = recycle\n", fmt.Sprintf("   recycle:repository = %s\n", recycleDir), "   recycle:keeptree = yes\n", "   recycle:versions = yes\n")
	}
	replacement := section{name: input.Name, lines: lines}
	found := false
	for i, s := range sections {
		if s.name == input.Name {
			sections[i] = replacement
			found = true
		}
	}
	if !found {
		sections = append(sections, replacement)
	}
	return render(sections), nil
}
func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}
func deleteShare(name string) string {
	sections := splitSections(read(smbConf))
	kept := make([]section, 0, len(sections))
	for _, s := range sections {
		if s.name != name {
			kept = append(kept, s)
		}
	}
	return render(kept)
}
func restore(input recycleEntry) (string, error) {
	shares, _, _ := state()
	var root string
	for _, share := range shares {
		if share.Name == input.Share && share.Recycle {
			root = share.Path
		}
	}
	if root == "" || input.Path == "" || filepath.IsAbs(input.Path) || strings.Contains(filepath.ToSlash(input.Path), "../") {
		return "", errors.New("invalid recycle entry")
	}
	base, _ := filepath.Abs(root)
	src, _ := filepath.Abs(filepath.Join(base, recycleDir, input.Path))
	dst, _ := filepath.Abs(filepath.Join(base, input.Path))
	if !strings.HasPrefix(src, base+string(os.PathSeparator)) || !strings.HasPrefix(dst, base+string(os.PathSeparator)) {
		return "", errors.New("invalid recycle path")
	}
	if _, err := os.Stat(dst); err == nil {
		return "", errors.New("destination already exists")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return "", err
	}
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

type response map[string]any

func jsonResponse(w http.ResponseWriter, status int, value any) {
	data, _ := json.Marshal(value)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
func decode(r *http.Request, value any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
func validate(w http.ResponseWriter) {
	out, serr, err := runHost("testparm", "-s")
	samba := response{"ok": err == nil, "stdout": out, "stderr": serr}
	out, serr, err = runHost("exportfs", "-v")
	jsonResponse(w, 200, response{"samba": samba, "nfs": response{"ok": err == nil, "stdout": out, "stderr": serr}})
}

func sambaUsers() []string {
	users := make([]string, 0)
	if out, _, err := runHost("pdbedit", "-L"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if name := strings.TrimSpace(strings.SplitN(line, ":", 2)[0]); name != "" {
				users = append(users, name)
			}
		}
	}
	if len(users) == 0 {
		if out, _, err := runHost("getent", "passwd"); err == nil {
			for _, line := range strings.Split(out, "\n") {
				parts := strings.Split(line, ":")
				if len(parts) >= 7 && parts[0] != "" {
					var uid int
					if _, err := fmt.Sscanf(parts[2], "%d", &uid); err == nil && uid >= minUserUID && !strings.Contains(parts[6], "nologin") {
						users = append(users, parts[0])
					}
				}
			}
		}
	}
	return users
}

func browse(path string) ([]browseEntry, error) {
	if path == "" {
		path = browseRoot
	}
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, browseRoot) || (clean != browseRoot && !strings.HasPrefix(clean, browseRoot+string(os.PathSeparator))) {
		return nil, fmt.Errorf("browse este permis doar în %s", browseRoot)
	}
	real, err := filepath.EvalSymlinks(clean)
	if err != nil || (real != browseRoot && !strings.HasPrefix(real, browseRoot+string(os.PathSeparator))) {
		return nil, errors.New("director invalid")
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		return nil, err
	}
	result := make([]browseEntry, 0)
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			result = append(result, browseEntry{Name: entry.Name(), Path: filepath.Join(clean, entry.Name())})
		}
	}
	return result, nil
}

func nfsTarget(path string) string {
	managerFile := filepath.Join(exportsD, nfsManagerFile)
	for _, file := range []string{exports, managerFile} {
		for _, line := range strings.Split(read(file), "\n") {
			if parts := strings.Fields(line); len(parts) > 0 && parts[0] == path {
				return file
			}
		}
	}
	return managerFile
}

func rewriteNFS(path, replacement string) error {
	target := nfsTarget(path)
	var lines []string
	for _, line := range strings.Split(read(target), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") && strings.HasPrefix(trimmed, path+" ") {
			continue
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	if replacement != "" {
		lines = append(lines, replacement)
	}
	return atomicWrite(target, strings.TrimSpace(strings.Join(lines, "\n"))+"\n")
}

func handler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	static := http.FileServer(http.Dir(staticDir))
	if r.Method == "GET" && path == "/health" {
		jsonResponse(w, 200, response{"ok": true})
		return
	}
	if r.Method == "GET" && path == "/api/settings.json" {
		settings := map[string]any{}
		if data, err := os.ReadFile(uiSettingsFile); err == nil {
			_ = json.Unmarshal(data, &settings)
		}
		jsonResponse(w, 200, response{"settings": settings})
		return
	}
	if r.Method == "GET" && path != "/" && !strings.HasPrefix(path, "/api/") {
		static.ServeHTTP(w, r)
		return
	}
	if r.Method == "GET" && path == "/api/state" {
		shares, nfs, recycle := state()
		jsonResponse(w, 200, response{"samba": shares, "nfs": nfs, "recycle": recycle})
		return
	}
	if r.Method == "GET" && path == "/api/validate" {
		mu.Lock()
		defer mu.Unlock()
		validate(w)
		return
	}
	if r.Method == "GET" && path == "/api/users" {
		jsonResponse(w, 200, response{"users": sambaUsers()})
		return
	}
	if r.Method == "GET" && path == "/api/browse" {
		entries, err := browse(r.URL.Query().Get("path"))
		if err != nil {
			jsonResponse(w, 400, response{"error": err.Error()})
			return
		}
		jsonResponse(w, 200, response{"entries": entries})
		return
	}
	if r.Method == "GET" && path == "/" {
		http.ServeFile(w, r, uiFile)
		return
	}
	if r.Method != "POST" {
		jsonResponse(w, 404, response{"error": "not found"})
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if path == "/api/settings" {
		patch := map[string]any{}
		if err := decode(r, &patch); err != nil {
			jsonResponse(w, 400, response{"error": err.Error()})
			return
		}
		settings := map[string]any{}
		if data, err := os.ReadFile(uiSettingsFile); err == nil {
			_ = json.Unmarshal(data, &settings)
		}
		for key, value := range patch {
			switch key {
			case "theme", "theme_style", "font_size", "poll_interval", "language":
				settings[key] = value
			}
		}
		if err := atomicWrite(uiSettingsFile, mustJSON(settings)); err != nil {
			jsonResponse(w, 400, response{"error": err.Error()})
			return
		}
		jsonResponse(w, 200, response{"ok": true})
		return
	}
	if path == "/api/users/create" {
		var input struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := decode(r, &input); err != nil || !namePattern.MatchString(input.Username) || len(input.Password) < 4 {
			jsonResponse(w, 400, response{"error": "username invalid sau parola prea scurtă (minim 4 caractere)"})
			return
		}
		if out, _, err := runHost("getent", "passwd", input.Username); err != nil || strings.TrimSpace(out) == "" {
			jsonResponse(w, 400, response{"error": "utilizatorul Linux nu există în /etc/passwd"})
			return
		}
		saved, err := backup()
		if err == nil {
			_, serr, e := runHostInput(input.Password+"\n"+input.Password+"\n", "smbpasswd", "-a", "-s", input.Username)
			if e != nil {
				err = fmt.Errorf("smbpasswd failed: %s", serr)
			}
		}
		if err != nil {
			jsonResponse(w, 400, response{"error": err.Error(), "backup": saved})
			return
		}
		jsonResponse(w, 200, response{"ok": true, "backup": saved})
		return
	}
	if path == "/api/users/delete" {
		var input struct {
			Username string `json:"username"`
		}
		if err := decode(r, &input); err != nil || !namePattern.MatchString(input.Username) {
			jsonResponse(w, 400, response{"error": "username invalid"})
			return
		}
		saved, err := backup()
		if err == nil {
			_, serr, e := runHost("smbpasswd", "-x", input.Username)
			if e != nil {
				err = fmt.Errorf("smbpasswd failed: %s", serr)
			}
		}
		if err != nil {
			jsonResponse(w, 400, response{"error": err.Error(), "backup": saved})
			return
		}
		jsonResponse(w, 200, response{"ok": true, "backup": saved})
		return
	}
	if path == "/api/samba/share" {
		var input sambaShare
		if err := decode(r, &input); err != nil {
			jsonResponse(w, 400, response{"error": err.Error()})
			return
		}
		saved, err := backup()
		if err == nil {
			var text string
			text, err = editShare(input)
			if err == nil {
				err = atomicWrite(smbConf, text)
			}
			if err == nil {
				_, sout, e := runHost("testparm", "-s")
				if e != nil {
					err = fmt.Errorf("testparm failed: %s", sout)
				}
			}
			if err == nil {
				_, sout, e := runHost("systemctl", "reload", smbService)
				if e != nil {
					err = fmt.Errorf("reload failed: %s", sout)
				}
			}
		}
		if err != nil {
			jsonResponse(w, 400, response{"error": err.Error(), "backup": saved})
			return
		}
		jsonResponse(w, 200, response{"ok": true, "backup": saved})
		return
	}
	if path == "/api/samba/delete" {
		var input struct {
			Name string `json:"name"`
		}
		if err := decode(r, &input); err != nil || input.Name == "" || strings.EqualFold(input.Name, "global") {
			jsonResponse(w, 400, response{"error": "invalid share name"})
			return
		}
		saved, err := backup()
		if err == nil {
			err = atomicWrite(smbConf, deleteShare(input.Name))
		}
		if err == nil {
			_, sout, e := runHost("testparm", "-s")
			if e != nil {
				err = fmt.Errorf("testparm failed: %s", sout)
			}
		}
		if err == nil {
			_, sout, e := runHost("systemctl", "reload", smbService)
			if e != nil {
				err = fmt.Errorf("reload failed: %s", sout)
			}
		}
		if err != nil {
			jsonResponse(w, 400, response{"error": err.Error(), "backup": saved})
			return
		}
		jsonResponse(w, 200, response{"ok": true, "backup": saved})
		return
	}
	if path == "/api/nfs/export" {
		var input nfsExport
		if err := decode(r, &input); err != nil || !strings.HasPrefix(input.Path, "/") {
			jsonResponse(w, 400, response{"error": "NFS path and clients are required"})
			return
		}
		if input.Client != "" {
			input.Client = strings.TrimSpace(input.Client)
			input.Options = strings.TrimSpace(input.Options)
			input.Clients = input.Client + input.Options
		}
		if input.Clients == "" {
			jsonResponse(w, 400, response{"error": "NFS client and options are required"})
			return
		}
		saved, err := backup()
		if err == nil {
			err = os.MkdirAll(exportsD, 0755)
		}
		if err == nil {
			err = rewriteNFS(input.Path, input.Path+" "+input.Clients)
		}
		if err == nil {
			_, sout, e := runHost("exportfs", "-ra")
			if e != nil {
				err = fmt.Errorf("exportfs failed: %s", sout)
			}
		}
		if err != nil {
			jsonResponse(w, 400, response{"error": err.Error(), "backup": saved})
			return
		}
		jsonResponse(w, 200, response{"ok": true, "backup": saved})
		return
	}
	if path == "/api/nfs/delete" {
		var input struct {
			Path string `json:"path"`
		}
		if err := decode(r, &input); err != nil || !strings.HasPrefix(input.Path, "/") {
			jsonResponse(w, 400, response{"error": "invalid NFS path"})
			return
		}
		saved, err := backup()
		if err == nil {
			err = rewriteNFS(input.Path, "")
		}
		if err == nil {
			_, serr, e := runHost("exportfs", "-ra")
			if e != nil {
				err = fmt.Errorf("exportfs failed: %s", serr)
			}
		}
		if err != nil {
			jsonResponse(w, 400, response{"error": err.Error(), "backup": saved})
			return
		}
		jsonResponse(w, 200, response{"ok": true, "backup": saved})
		return
	}
	if path == "/api/recycle/restore" {
		var input recycleEntry
		if err := decode(r, &input); err != nil {
			jsonResponse(w, 400, response{"error": err.Error()})
			return
		}
		destination, err := restore(input)
		if err != nil {
			jsonResponse(w, 400, response{"error": err.Error()})
			return
		}
		jsonResponse(w, 200, response{"ok": true, "destination": destination})
		return
	}
	jsonResponse(w, 404, response{"error": "not found"})
}
func main() {
	http.HandleFunc("/", handler)
	port := getenv("PORT", "")
	if port == "" || uiFile == "" || staticDir == "" || uiSettingsFile == "" || smbConf == "" || exports == "" || exportsD == "" || backups == "" || backupDisplay == "" || browseRoot == "" || recycleDir == "" || nfsManagerFile == "" || sambaDB == "" || smbService == "" || nsenterPID < 1 || fileUID < 0 || fileGID < 0 || maxRecycle < 1 {
		log.Fatal("required configuration is missing; configure the share-manager environment in compose.yaml")
	}
	http.ListenAndServe(":"+port, nil)
}
