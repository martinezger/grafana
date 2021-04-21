package installer

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/fatih/color"

	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/util/errutil"
)

type Installer struct {
	retryCount int

	httpClient          http.Client
	httpClientNoTimeout http.Client
	log                 log.Logger
}

const (
	permissionsDeniedMessage = "could not create %q, permission denied, make sure you have write access to plugin dir"
)

var (
	ErrNotFoundError = errors.New("404 not found error")
	reGitBuild       = regexp.MustCompile("^[a-zA-Z0-9_.-]*/")
	grafanaVersion   = setting.BuildVersion
)

type BadRequestError struct {
	Message string
	Status  string
}

func (e *BadRequestError) Error() string {
	if len(e.Message) > 0 {
		return fmt.Sprintf("%s: %s", e.Status, e.Message)
	}
	return e.Status
}

func New(skipTLSVerify bool, logger log.Logger) *Installer {
	return &Installer{
		httpClient:          makeHttpClient(skipTLSVerify, 10*time.Second),
		httpClientNoTimeout: makeHttpClient(skipTLSVerify, 10*time.Second),
		log:                 logger,
	}
}

func (g *Installer) Install(pluginID, version, pluginsDir, pluginZipURL, pluginRepoURL string) error {
	isInternal := false

	var checksum string
	if pluginZipURL == "" {
		if strings.HasPrefix(pluginID, "grafana-") {
			// At this point the plugin download is going through grafana.com API and thus the name is validated.
			// Checking for grafana prefix is how it is done there so no 3rd party plugin should have that prefix.
			// You can supply custom plugin name and then set custom download url to 3rd party plugin but then that
			// is up to the user to know what she is doing.
			isInternal = true
		}
		plugin, err := g.getPluginMetadataFromPluginRepo(pluginID, pluginRepoURL)
		if err != nil {
			return err
		}

		v, err := selectVersion(&plugin, version)
		if err != nil {
			return err
		}

		if version == "" {
			version = v.Version
		}
		pluginZipURL = fmt.Sprintf("%s/%s/versions/%s/download",
			pluginRepoURL,
			pluginID,
			version,
		)

		// Plugins which are downloaded just as sourcecode zipball from github do not have checksum
		if v.Arch != nil {
			archMeta, exists := v.Arch[osAndArchString()]
			if !exists {
				archMeta = v.Arch["any"]
			}
			checksum = archMeta.SHA256
		}
	}
	g.log.Info(fmt.Sprintf("installing %v @ %v\n", pluginID, version))
	g.log.Info(fmt.Sprintf("from: %v\n", pluginZipURL))
	g.log.Info(fmt.Sprintf("into: %v\n", pluginsDir))
	g.log.Info("\n")

	// Create temp file for downloading zip file
	tmpFile, err := ioutil.TempFile("", "*.zip")
	if err != nil {
		return errutil.Wrap("failed to create temporary file", err)
	}
	defer func() {
		if err := os.Remove(tmpFile.Name()); err != nil {
			g.log.Warn("Failed to remove temporary file", "file", tmpFile.Name(), "err", err)
		}
	}()

	err = g.DownloadFile(pluginID, tmpFile, pluginZipURL, checksum)
	if err != nil {
		if err := tmpFile.Close(); err != nil {
			g.log.Warn("Failed to close file", "err", err)
		}
		return errutil.Wrap("failed to download plugin archive", err)
	}
	err = tmpFile.Close()
	if err != nil {
		return errutil.Wrap("failed to close tmp file", err)
	}

	err = g.extractFiles(tmpFile.Name(), pluginID, pluginsDir, isInternal)
	if err != nil {
		return errutil.Wrap("failed to extract plugin archive", err)
	}

	g.log.Info(fmt.Sprintf("%s Installed %s successfully \n", color.GreenString("✔"), pluginID))

	// download dependency plugins
	res, _ := toPluginDTO(pluginsDir, pluginID)
	for _, dep := range res.Dependencies.Plugins {
		if err := g.Install(dep.ID, normalizeVersion(dep.Version), pluginsDir, "", pluginRepoURL); err != nil {
			return errutil.Wrapf(err, "failed to install plugin '%s'", dep.ID)
		}

		g.log.Info(fmt.Sprintf("Installed dependency: %v ✔\n", dep.ID))
	}

	return err
}

func (g *Installer) DownloadFile(pluginID string, tmpFile *os.File, url string, checksum string) (err error) {
	// Try handling URL as a local file path first
	if _, err := os.Stat(url); err == nil {
		// We can ignore this gosec G304 warning since `url` stems from command line flag "pluginUrl". If the
		// user shouldn't be able to read the file, it should be handled through filesystem permissions.
		// nolint:gosec
		f, err := os.Open(url)
		if err != nil {
			return errutil.Wrap("Failed to read plugin archive", err)
		}
		_, err = io.Copy(tmpFile, f)
		if err != nil {
			return errutil.Wrap("Failed to copy plugin archive", err)
		}
		return nil
	}

	g.retryCount = 0

	defer func() {
		if r := recover(); r != nil {
			g.retryCount++
			if g.retryCount < 3 {
				g.log.Info("Failed downloading. Will retry once.")
				err = tmpFile.Truncate(0)
				if err != nil {
					return
				}
				_, err = tmpFile.Seek(0, 0)
				if err != nil {
					return
				}
				err = g.DownloadFile(pluginID, tmpFile, url, checksum)
			} else {
				g.retryCount = 0
				failure := fmt.Sprintf("%v", r)
				if failure == "runtime error: makeslice: len out of range" {
					err = fmt.Errorf("corrupt HTTP response from source, please try again")
				} else {
					panic(r)
				}
			}
		}
	}()

	g.log.Info("Sending request to download plugin", "url", url)

	// Using no timeout here as some plugins can be bigger and smaller timeout would prevent to download a plugin on
	// slow network. As this is CLI operation hanging is not a big of an issue as user can just abort.
	bodyReader, err := g.sendRequestWithoutTimeout(url)
	if err != nil {
		return errutil.Wrap("Failed to send request", err)
	}
	defer func() {
		if err := bodyReader.Close(); err != nil {
			g.log.Warn("Failed to close body", "err", err)
		}
	}()

	w := bufio.NewWriter(tmpFile)
	h := sha256.New()
	if _, err = io.Copy(w, io.TeeReader(bodyReader, h)); err != nil {
		return errutil.Wrap("failed to compute SHA256 checksum", err)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("failed to write to %q: %w", tmpFile.Name(), err)
	}
	if len(checksum) > 0 && checksum != fmt.Sprintf("%x", h.Sum(nil)) {
		return fmt.Errorf("expected SHA256 checksum does not match the downloaded archive - please contact security@grafana.com")
	}
	return nil
}

func (g *Installer) getPluginMetadataFromPluginRepo(pluginID, pluginRepoURL string) (Plugin, error) {
	g.log.Info(fmt.Sprintf("getting %v metadata from GCOM\n", pluginID))
	body, err := g.sendRequestGetBytes(pluginRepoURL, "repo", pluginID)
	if err != nil {
		if errors.Is(err, ErrNotFoundError) {
			return Plugin{}, errutil.Wrap(
				fmt.Sprintf("Failed to find requested plugin, check if the plugin_id (%s) is correct", pluginID), err)
		}
		return Plugin{}, errutil.Wrap("Failed to send request", err)
	}

	var data Plugin
	err = json.Unmarshal(body, &data)
	if err != nil {
		g.log.Info("Failed to unmarshal plugin repo response error:", err)
		return Plugin{}, err
	}

	return data, nil
}

func (g *Installer) sendRequestGetBytes(URL string, subPaths ...string) ([]byte, error) {
	bodyReader, err := g.sendRequest(URL, subPaths...)
	if err != nil {
		return []byte{}, err
	}
	defer func() {
		if err := bodyReader.Close(); err != nil {
			g.log.Warn("Failed to close stream", "err", err)
		}
	}()
	return ioutil.ReadAll(bodyReader)
}

func (g *Installer) sendRequest(URL string, subPaths ...string) (io.ReadCloser, error) {
	req, err := g.createRequest(URL, subPaths...)
	if err != nil {
		return nil, err
	}

	res, err := g.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	return g.handleResponse(res)
}

func (g *Installer) sendRequestWithoutTimeout(URL string, subPaths ...string) (io.ReadCloser, error) {
	req, err := g.createRequest(URL, subPaths...)
	if err != nil {
		return nil, err
	}

	res, err := g.httpClientNoTimeout.Do(req)
	if err != nil {
		return nil, err
	}
	return g.handleResponse(res)
}

func (g *Installer) createRequest(URL string, subPaths ...string) (*http.Request, error) {
	u, err := url.Parse(URL)
	if err != nil {
		return nil, err
	}

	for _, v := range subPaths {
		u.Path = path.Join(u.Path, v)
	}

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("grafana-version", grafanaVersion)
	req.Header.Set("grafana-os", runtime.GOOS)
	req.Header.Set("grafana-arch", runtime.GOARCH)
	req.Header.Set("User-Agent", "grafana "+grafanaVersion)

	return req, err
}

func (g *Installer) handleResponse(res *http.Response) (io.ReadCloser, error) {
	if res.StatusCode == 404 {
		return nil, ErrNotFoundError
	}

	if res.StatusCode/100 != 2 && res.StatusCode/100 != 4 {
		return nil, fmt.Errorf("API returned invalid status: %s", res.Status)
	}

	if res.StatusCode/100 == 4 {
		body, err := ioutil.ReadAll(res.Body)
		defer func() {
			if err := res.Body.Close(); err != nil {
				g.log.Warn("Failed to close response body", "err", err)
			}
		}()
		if err != nil || len(body) == 0 {
			return nil, &BadRequestError{Status: res.Status}
		}
		var message string
		var jsonBody map[string]string
		err = json.Unmarshal(body, &jsonBody)
		if err != nil || len(jsonBody["message"]) == 0 {
			message = string(body)
		} else {
			message = jsonBody["message"]
		}
		return nil, &BadRequestError{Status: res.Status, Message: message}
	}

	return res.Body, nil
}

func makeHttpClient(skipTLSVerify bool, timeout time.Duration) http.Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: skipTLSVerify,
		},
	}

	return http.Client{
		Timeout:   timeout,
		Transport: tr,
	}
}

func normalizeVersion(version string) string {
	normalized := strings.ReplaceAll(version, " ", "")
	if strings.HasPrefix(normalized, "^") || strings.HasPrefix(normalized, "v") {
		return normalized[1:]
	}

	return normalized
}

// selectVersion returns latest version if none is specified or the specified version. If the version string is not
// matched to existing version it errors out. It also errors out if version that is matched is not available for current
// os and platform. It expects plugin.Versions to be sorted so the newest version is first.
func selectVersion(plugin *Plugin, version string) (*Version, error) {
	var ver Version

	latestForArch := latestSupportedVersion(plugin)
	if latestForArch == nil {
		return nil, fmt.Errorf("plugin is not supported on your architecture and OS")
	}

	if version == "" {
		return latestForArch, nil
	}
	for _, v := range plugin.Versions {
		if v.Version == version {
			ver = v
			break
		}
	}

	if len(ver.Version) == 0 {
		return nil, fmt.Errorf("could not find the version you're looking for")
	}

	if !supportsCurrentArch(&ver) {
		return nil, fmt.Errorf(
			"the version you want is not supported on your architecture and OS, latest suitable version is %s",
			latestForArch.Version)
	}

	return &ver, nil
}

func osAndArchString() string {
	osString := strings.ToLower(runtime.GOOS)
	arch := runtime.GOARCH
	return osString + "-" + arch
}

func supportsCurrentArch(version *Version) bool {
	if version.Arch == nil {
		return true
	}
	for arch := range version.Arch {
		if arch == osAndArchString() || arch == "any" {
			return true
		}
	}
	return false
}

func latestSupportedVersion(plugin *Plugin) *Version {
	for _, v := range plugin.Versions {
		ver := v
		if supportsCurrentArch(&ver) {
			return &ver
		}
	}
	return nil
}

func (g *Installer) extractFiles(archiveFile string, pluginID string, dstDir string, allowSymlinks bool) error {
	var err error
	dstDir, err = filepath.Abs(dstDir)
	if err != nil {
		return err
	}
	g.log.Debug(fmt.Sprintf("Extracting archive %q to %q...\n", archiveFile, dstDir))

	existingInstallDir := filepath.Join(dstDir, pluginID)
	if _, err := os.Stat(existingInstallDir); !os.IsNotExist(err) {
		err = os.RemoveAll(existingInstallDir)
		if err != nil {
			return err
		}

		g.log.Info(fmt.Sprintf("Removed existing installation of %s\n\n", pluginID))
	}

	r, err := zip.OpenReader(archiveFile)
	if err != nil {
		return err
	}
	for _, zf := range r.File {
		if filepath.IsAbs(zf.Name) || strings.HasPrefix(zf.Name, ".."+string(filepath.Separator)) {
			return fmt.Errorf(
				"archive member %q tries to write outside of plugin directory: %q, this can be a security risk",
				zf.Name, dstDir)
		}

		dstPath := filepath.Clean(filepath.Join(dstDir, removeGitBuildFromName(pluginID, zf.Name)))

		if zf.FileInfo().IsDir() {
			// We can ignore gosec G304 here since it makes sense to give all users read access
			// nolint:gosec
			if err := os.MkdirAll(dstPath, 0755); err != nil {
				if os.IsPermission(err) {
					return fmt.Errorf(permissionsDeniedMessage, dstPath)
				}

				return err
			}

			continue
		}

		// Create needed directories to extract file
		// We can ignore gosec G304 here since it makes sense to give all users read access
		// nolint:gosec
		if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
			return errutil.Wrap("failed to create directory to extract plugin files", err)
		}

		if isSymlink(zf) {
			if !allowSymlinks {
				g.log.Warn(fmt.Sprintf("%v: plugin archive contains a symlink, which is not allowed. Skipping \n", zf.Name))
				continue
			}
			if err := extractSymlink(zf, dstPath); err != nil {
				g.log.Info(fmt.Sprintf("Failed to extract symlink: %v \n", err))
				continue
			}
			continue
		}

		if err := extractFile(zf, dstPath); err != nil {
			return errutil.Wrap("failed to extract file", err)
		}
	}

	return nil
}

func isSymlink(file *zip.File) bool {
	return file.Mode()&os.ModeSymlink == os.ModeSymlink
}

func extractSymlink(file *zip.File, filePath string) error {
	// symlink target is the contents of the file
	src, err := file.Open()
	if err != nil {
		return errutil.Wrap("failed to extract file", err)
	}
	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, src); err != nil {
		return errutil.Wrap("failed to copy symlink contents", err)
	}
	if err := os.Symlink(strings.TrimSpace(buf.String()), filePath); err != nil {
		return errutil.Wrapf(err, "failed to make symbolic link for %v", filePath)
	}
	return nil
}

func extractFile(file *zip.File, filePath string) (err error) {
	fileMode := file.Mode()
	// This is entry point for backend plugins so we want to make them executable
	if strings.HasSuffix(filePath, "_linux_amd64") || strings.HasSuffix(filePath, "_darwin_amd64") {
		fileMode = os.FileMode(0755)
	}

	// We can ignore the gosec G304 warning on this one, since the variable part of the file path stems
	// from command line flag "pluginsDir", and the only possible damage would be writing to the wrong directory.
	// If the user shouldn't be writing to this directory, they shouldn't have the permission in the file system.
	// nolint:gosec
	dst, err := os.OpenFile(filePath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf(permissionsDeniedMessage, filePath)
		}

		unwrappedError := errors.Unwrap(err)
		if unwrappedError != nil && strings.EqualFold(unwrappedError.Error(), "text file busy") {
			return fmt.Errorf("file %q is in use - please stop Grafana, install the plugin and restart Grafana", filePath)
		}

		return errutil.Wrap("failed to open file", err)
	}
	defer func() {
		err = dst.Close()
	}()

	src, err := file.Open()
	if err != nil {
		return errutil.Wrap("failed to extract file", err)
	}
	defer func() {
		err = src.Close()
	}()

	_, err = io.Copy(dst, src)
	return err
}

func removeGitBuildFromName(pluginID, filename string) string {
	return reGitBuild.ReplaceAllString(filename, pluginID+"/")
}

func toPluginDTO(pluginDir, pluginID string) (InstalledPlugin, error) {
	distPluginDataPath := filepath.Join(pluginDir, pluginID, "dist", "plugin.json")

	data, err := ioutil.ReadFile(distPluginDataPath)
	if err != nil {
		pluginDataPath := filepath.Join(pluginDir, pluginID, "plugin.json")
		data, err = ioutil.ReadFile(pluginDataPath)
		if err != nil {
			return InstalledPlugin{}, errors.New("Could not find dist/plugin.json or plugin.json on  " + pluginID + " in " + pluginDir)
		}
	}

	res := InstalledPlugin{}
	if err := json.Unmarshal(data, &res); err != nil {
		return res, err
	}

	if res.Info.Version == "" {
		res.Info.Version = "0.0.0"
	}

	if res.ID == "" {
		return InstalledPlugin{}, errors.New("could not find plugin " + pluginID + " in " + pluginDir)
	}

	return res, nil
}
