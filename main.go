package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/go-plugins-helpers/volume"
	"github.com/sirupsen/logrus"
)

const (
	socketAddress = "/run/docker/plugins/jfs.sock"

	// Community Edition CLI (metaurl-based), pinned via JUICEFS_CE_VERSION.
	ceCliPath = "/bin/juicefs"

	// Enterprise/Cloud CLI (token-based), downloaded from JUICEFS_EE_URL.
	eeCliPath = "/usr/bin/juicefs"
)

// Detect legacy/new CLI behaviors to keep compatibility across versions.
func isAuthUnsupported(output string) bool {
	out := strings.ToLower(output)
	return strings.Contains(out, "no help topic for 'auth'") ||
		(strings.Contains(out, "unknown") && strings.Contains(out, "auth")) ||
		strings.Contains(out, "unknown option: --token") ||
		strings.Contains(out, "unknown flag: --token") ||
		strings.Contains(out, "flag provided but not defined: --token")
}

func canonicalize(k string) string {
	switch k {
	case "accesskey":
		return "access-key"
	case "accesskey2":
		return "access-key2"
	case "secretkey":
		return "secret-key"
	case "secretkey2":
		return "secret-key2"
	default:
		return k
	}
}

// sanitizeOutput replaces any sensitive values with "****" so we can safely
// log JuiceFS CLI output.
func sanitizeOutput(out string, secrets []string) string {
	redacted := out
	for _, s := range secrets {
		if s == "" {
			continue
		}
		redacted = strings.ReplaceAll(redacted, s, "****")
	}
	return redacted
}

// waitForMountReady polls the mountpoint until it becomes a JuiceFS mount
// (root inode == 1) or times out.
func waitForMountReady(mountpoint string) error {
	touch := exec.Command("touch", filepath.Join(mountpoint, ".juicefs"))
	lastErr := fmt.Errorf("mountpoint %s did not become ready", mountpoint)

	for attempt := 0; attempt < 10; attempt++ {
		fi, err := os.Lstat(mountpoint)
		if err == nil {
			stat, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				return logError("Not a syscall.Stat_t")
			}
			if stat.Ino == 1 {
				if err := touch.Run(); err == nil {
					return nil
				}
				lastErr = err
			} else {
				lastErr = fmt.Errorf("mountpoint %s not yet a JuiceFS mount (ino=%d)", mountpoint, stat.Ino)
			}
		} else {
			lastErr = err
		}

		logrus.Debugf("Error in attempt %d waiting for %s: %#v", attempt+1, mountpoint, lastErr)
		time.Sleep(time.Second)
	}

	return logError(lastErr.Error())
}

// isJuiceFSMountedRoot checks if the given path is a JuiceFS mount root by
// looking for inode 1. This is used only for diagnostics; bind-mount mode
// should be controlled via explicit options, not heuristics.
func isJuiceFSMountedRoot(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return stat.Ino == 1
}

type jfsVolume struct {
	Name        string
	Options     map[string]string
	Source      string
	Mountpoint  string
	connections int
}

type jfsDriver struct {
	sync.RWMutex

	root      string
	statePath string
	volumes   map[string]*jfsVolume
}

func newJfsDriver(root string) (*jfsDriver, error) {
	logrus.WithField("method", "newJfsDriver").Debug(root)

	d := &jfsDriver{
		root:      filepath.Join(root, "volumes"),
		statePath: filepath.Join(root, "state", "jfs-state.json"),
		volumes:   map[string]*jfsVolume{},
	}

	if data, err := ioutil.ReadFile(d.statePath); err != nil {
		if os.IsNotExist(err) {
			logrus.WithField("statePath", d.statePath).Debug("no state found")
		} else {
			return nil, err
		}
	} else {
		if err := json.Unmarshal(data, &d.volumes); err != nil {
			return nil, err
		}
	}

	return d, nil
}

func (d *jfsDriver) saveState() {
	data, err := json.Marshal(d.volumes)
	if err != nil {
		logrus.WithField("statePath", d.statePath).Error(err)
	}

	if err := ioutil.WriteFile(d.statePath, data, 0600); err != nil {
		logrus.WithField("saveState", d.statePath).Error(err)
	}
}

func ceMount(v *jfsVolume) error {
	options := map[string]string{}
	format := exec.Command(ceCliPath, "format", "--no-update")
	for k, val := range v.Options {
		if k == "env" {
			format.Env = append(os.Environ(), strings.Split(val, ",")...)
			logrus.Debugf("modified env for volume %s: %v", v.Name, format.Env)
			continue
		}
		options[k] = val
	}
	formatOptions := []string{
		"block-size",
		"compress",
		"shards",
		"storage",
		"bucket",
		"access-key",
		"secret-key",
		"encrypt-rsa-key",
		"trash-days",
	}
	for _, formatOption := range formatOptions {
		val, ok := options[formatOption]
		if !ok {
			continue
		}
		format.Args = append(format.Args, fmt.Sprintf("--%s=%s", formatOption, val))
		delete(options, formatOption)
	}
	format.Args = append(format.Args, v.Source, v.Name)
	logrus.Debug(format)
	if out, err := format.CombinedOutput(); err != nil {
		logrus.Errorf("juicefs format error: %s", out)
		return logError(err.Error())
	}

	// options left for `juicefs mount`
	mount := exec.Command(ceCliPath, "mount")
	// ensure we don't attempt to auto-download helper and prefer bundled one
	mount.Env = append(os.Environ(), "JFS_NO_UPDATE=1")
	if _, err := os.Stat("/bin/jfsmount"); err == nil {
		mount.Env = append(mount.Env, "JFS_MOUNT_BIN=/bin/jfsmount")
	}
	// run mount in background to avoid blocking and ensure child lifecycle isn't tied to plugin process
	mount.Args = append(mount.Args, "-d")
	mountFlags := []string{
		"cache-partial-only",
		"enable-xattr",
		"no-syslog",
		"no-usage-report",
		"writeback",
	}
	for _, mountFlag := range mountFlags {
		_, ok := options[mountFlag]
		if !ok {
			continue
		}
		mount.Args = append(mount.Args, fmt.Sprintf("--%s", mountFlag))
		delete(options, mountFlag)
	}
	for mountOption, val := range options {
		mount.Args = append(mount.Args, fmt.Sprintf("--%s=%s", mountOption, val))
	}
	mount.Args = append(mount.Args, v.Source, v.Mountpoint)
	logrus.Debug(mount)
	// Start mount in background to avoid waitid/ECHILD issues when the helper daemonizes.
	if err := mount.Start(); err != nil {
		return logError(err.Error())
	}

	return waitForMountReady(v.Mountpoint)
}

func eeMount(v *jfsVolume) error {
	// Copy options so we can safely mutate them.
	mountOpts := map[string]string{}
	for k, val := range v.Options {
		mountOpts[k] = val
	}

	// Build environment. "env" option is used only to inject env vars, not as a CLI flag.
	env := os.Environ()
	if envOpt, ok := mountOpts["env"]; ok && envOpt != "" {
		env = append(env, strings.Split(envOpt, ",")...)
		delete(mountOpts, "env")
		logrus.Debugf("modified env for volume %s: %v", v.Name, env)
	}

	// Secrets for log redaction.
	secrets := []string{
		mountOpts["token"],
		mountOpts["access-key"],
		mountOpts["accesskey"],
		mountOpts["access-key2"],
		mountOpts["accesskey2"],
		mountOpts["secret-key"],
		mountOpts["secretkey"],
		mountOpts["secret-key2"],
		mountOpts["secretkey2"],
	}

	// Map storage credentials to environment variables instead of CLI flags.
	// This keeps them out of logs and avoids CLI option changes breaking mounts.
	if val, ok := mountOpts["access-key"]; ok && val != "" {
		env = append(env, "ACCESS_KEY="+val)
	}
	if val, ok := mountOpts["accesskey"]; ok && val != "" {
		env = append(env, "ACCESS_KEY="+val)
	}
	if val, ok := mountOpts["access-key2"]; ok && val != "" {
		env = append(env, "ACCESS_KEY2="+val)
	}
	if val, ok := mountOpts["accesskey2"]; ok && val != "" {
		env = append(env, "ACCESS_KEY2="+val)
	}
	if val, ok := mountOpts["secret-key"]; ok && val != "" {
		env = append(env, "SECRET_KEY="+val)
	}
	if val, ok := mountOpts["secretkey"]; ok && val != "" {
		env = append(env, "SECRET_KEY="+val)
	}
	if val, ok := mountOpts["secret-key2"]; ok && val != "" {
		env = append(env, "SECRET_KEY2="+val)
	}
	if val, ok := mountOpts["secretkey2"]; ok && val != "" {
		env = append(env, "SECRET_KEY2="+val)
	}

		// ---- EE auth: juicefs auth NAME --token=... ----
		authToken := ""
		if val, ok := mountOpts["token"]; ok && val != "" {
			authToken = val
		}
		auth := exec.Command(eeCliPath, "auth", v.Name)
		auth.Env = env
		if authToken != "" {
			auth.Args = append(auth.Args, fmt.Sprintf("--token=%s", authToken))
		}
		logrus.Debug(auth)
		if out, err := auth.CombinedOutput(); err != nil {
			msg := sanitizeOutput(string(bytes.TrimSpace(out)), secrets)
			return logError("juicefs auth failed for volume %s: %s", v.Name, msg)
		}
	
		// ---- EE mount: juicefs mount NAME MOUNTPOINT [options] ----

	mount := exec.Command(eeCliPath, "mount", v.Name, v.Mountpoint)
	// do not auto-download jfsmount; prefer bundled helper if present
	mount.Env = append(env, "JFS_NO_UPDATE=1")
	if _, err := os.Stat("/bin/jfsmount"); err == nil {
		mount.Env = append(mount.Env, "JFS_MOUNT_BIN=/bin/jfsmount")
	}
	// run mount in background for EE
	mount.Args = append(mount.Args, "-d")

	mountFlags := []string{
		"external",
		"internal",
		"gc",
		"dry",
		"flip",
		"no-sync",
		"allow-other",
		"allow-root",
		"enable-xattr",
	}

	// Normalize option names for mount.
	norm := map[string]string{}
	for k, val := range mountOpts {
		norm[canonicalize(k)] = val
	}
	mountOpts = norm

		// Capture token separately for potential future use; current CLI flow
		// only requires it during auth, not mount.
	token := ""
	if val, ok := mountOpts["token"]; ok && val != "" {
		token = val
		delete(mountOpts, "token")
	}

	// Object storage credentials belong in env, not as `mount` flags.
	// Strip all storage-related options before building the mount args.
	for _, k := range []string{
		"access-key", "accesskey", "access-key2", "accesskey2",
		"secret-key", "secretkey", "secret-key2", "secretkey2",
		"bucket", "bucket2",
		"storage",
	} {
		delete(mountOpts, k)
	}

	// Append flags and k=v options
	for _, mountFlag := range mountFlags {
		if _, ok := mountOpts[mountFlag]; ok {
			mount.Args = append(mount.Args, fmt.Sprintf("--%s", mountFlag))
			delete(mountOpts, mountFlag)
		}
	}
	for k, val := range mountOpts {
		mount.Args = append(mount.Args, fmt.Sprintf("--%s=%s", k, val))
	}
	if token != "" {
		mount.Args = append(mount.Args, fmt.Sprintf("--token=%s", token))
	}
	logrus.Debug(mount)

	// Capture output in the background so we can log errors (sanitized) without blocking.
	stdout, _ := mount.StdoutPipe()
	stderr, _ := mount.StderrPipe()

	if err := mount.Start(); err != nil {
		return logError("failed to start juicefs mount for volume %s: %v", v.Name, err)
	}

	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, io.MultiReader(stdout, stderr))
		if err := mount.Wait(); err != nil {
			msg := sanitizeOutput(buf.String(), secrets)
			// When the helper daemonizes, Wait can return errors like ECHILD; treat as debug.
			logrus.Debugf("juicefs mount process for volume %s exited with error (may be benign if daemonized): %s", v.Name, msg)
		}
	}()

	// Finally, poll for the mount to become ready.
	return waitForMountReady(v.Mountpoint)
}

func mountVolume(v *jfsVolume) error {
	fi, err := os.Lstat(v.Mountpoint)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(v.Mountpoint, 0755); err != nil {
			return logError(err.Error())
		}
	} else if err != nil {
		return logError(err.Error())
	}

	if fi != nil && !fi.IsDir() {
		return logError("%v already exist and it's not a directory", v.Mountpoint)
	}

	if !strings.Contains(v.Source, "://") {
		return eeMount(v)
	}
	return ceMount(v)
}

func umountVolume(v *jfsVolume) error {
	cmd := exec.Command("umount", v.Mountpoint)
	logrus.Debug(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		logrus.Errorf("juicefs umount error: %s", out)
		return logError(err.Error())
	}
	return nil
}

func (d *jfsDriver) Create(r *volume.CreateRequest) error {
	logrus.WithField("method", "create").Debugf("%#v", r)

	d.Lock()
	defer d.Unlock()

	v := &jfsVolume{
		Options: map[string]string{},
	}

	for key, val := range r.Options {
		switch key {
		case "name":
			v.Name = val
		case "metaurl":
			v.Source = val
			if !strings.Contains(v.Source, "://") {
				// Default scheme of meta URL is redis://
				v.Source = "redis://" + v.Source
			}
		default:
			v.Options[key] = val
		}
	}

	if v.Name == "" {
		return logError("'name' option required")
	}
	if v.Source == "" {
		v.Source = v.Name
	}

	v.Mountpoint = filepath.Join(d.root, r.Name)
	d.volumes[r.Name] = v

	d.saveState()
	return nil
}

func (d *jfsDriver) Remove(r *volume.RemoveRequest) error {
	logrus.WithField("method", "remove").Debugf("%#v", r)

	d.Lock()
	defer d.Unlock()

	v, ok := d.volumes[r.Name]

	if !ok {
		return logError("volume %s not found", r.Name)
	}

	if v.connections != 0 {
		return logError("volume %s is in use", r.Name)
	}

	if err := os.Remove(v.Mountpoint); err != nil {
		// Be tolerant when the mountpoint directory is already gone
		// so that probe/test volumes can be cleaned up without errors.
		if !os.IsNotExist(err) {
			return logError(err.Error())
		}
	}

	delete(d.volumes, r.Name)
	d.saveState()
	return nil
}

func (d *jfsDriver) Path(r *volume.PathRequest) (*volume.PathResponse, error) {
	logrus.WithField("method", "path").Debugf("%#v", r)

	d.RLock()
	defer d.RUnlock()

	v, ok := d.volumes[r.Name]
	if !ok {
		return &volume.PathResponse{}, logError("volume %s not found", r.Name)
	}

	return &volume.PathResponse{Mountpoint: v.Mountpoint}, nil
}

func (d *jfsDriver) Mount(r *volume.MountRequest) (*volume.MountResponse, error) {
	logrus.WithField("method", "mount").Debugf("%#v", r)

	v, ok := d.volumes[r.Name]
	if !ok {
		return &volume.MountResponse{}, logError("volume %s not found", r.Name)
	}

	err := mountVolume(v)
	if err != nil {
		return &volume.MountResponse{}, logError("failed to mount %s: %s", r.Name, err)
	}

	v.connections++
	return &volume.MountResponse{Mountpoint: v.Mountpoint}, nil
}

func (d *jfsDriver) Unmount(r *volume.UnmountRequest) error {
	logrus.WithField("method", "umount").Debugf("%#v", r)

	v, ok := d.volumes[r.Name]
	if !ok {
		return logError("volume %s not found", r.Name)
	}

	if err := umountVolume(v); err != nil {
		return logError("failed to umount %s: %s", r.Name, err)
	}

	v.connections--
	return nil
}

func (d *jfsDriver) Get(r *volume.GetRequest) (*volume.GetResponse, error) {
	logrus.WithField("method", "get").Debugf("%#v", r)

	d.Lock()
	defer d.Unlock()

	v, ok := d.volumes[r.Name]
	if !ok {
		return &volume.GetResponse{}, logError("volume %s not found", r.Name)
	}

	return &volume.GetResponse{Volume: &volume.Volume{Name: r.Name, Mountpoint: v.Mountpoint}}, nil
}

func (d *jfsDriver) List() (*volume.ListResponse, error) {
	logrus.WithField("method", "list").Debugf("")

	d.Lock()
	defer d.Unlock()

	var vols []*volume.Volume
	for name, v := range d.volumes {
		vols = append(vols, &volume.Volume{Name: name, Mountpoint: v.Mountpoint})
	}
	return &volume.ListResponse{Volumes: vols}, nil
}

func (d *jfsDriver) Capabilities() *volume.CapabilitiesResponse {
	logrus.WithField("method", "capabilities").Debugf("")

	return &volume.CapabilitiesResponse{Capabilities: volume.Capability{Scope: "local"}}
}

func logError(format string, args ...interface{}) error {
	logrus.Errorf(format, args...)
	return fmt.Errorf(format, args...)
}

func main() {
    debug := os.Getenv("DEBUG")
    if ok, _ := strconv.ParseBool(debug); ok {
        logrus.SetLevel(logrus.DebugLevel)
    }

	d, err := newJfsDriver("/jfs")
	if err != nil {
		logrus.Fatal(err)
	}
	h := volume.NewHandler(d)
	logrus.Infof("listening on %s", socketAddress)
	logrus.Error(h.ServeUnix(socketAddress, 0))
}
