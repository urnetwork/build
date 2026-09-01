// Command acceptance drives the locally built Linux or Windows service against
// main and emits one machine-readable result after every check succeeds.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/urnetwork/build/all/acceptance/authcases"
	"github.com/urnetwork/build/all/acceptance/testconfig"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

const (
	apiUrl              = "https://api.bringyour.com"
	maxApiResponseBytes = 1024 * 1024
)

// Holds validated command-line inputs for one platform campaign.
type options struct {
	Credentials        string
	Tests              string
	Fixture            string
	ActiveClient       string
	PeerProviderClient string
	StateDir           string
	SdkVersion         string
	AppVersion         string
	ServiceVersion     string
	Repeat             int

	ProviderMode            bool
	ProviderReady           string
	ProviderStop            string
	ProviderResult          string
	ProviderEgressInterface string
	ProviderEgressIndex     uint
}

// Records the changed-egress proof from every requested repetition.
type acceptanceResult struct {
	Ok          bool               `json:"ok"`
	Platform    string             `json:"platform"`
	Repetitions int                `json:"repetitions"`
	BeforeIps   []string           `json:"before_ips"`
	AfterIps    []string           `json:"after_ips"`
	PeerToPeer  []peerToPeerResult `json:"peer_to_peer"`
	AuthCases   []authcases.Result `json:"auth_cases"`
}

// Decodes the common error payload returned by direct cleanup calls.
type apiError struct {
	Message string `json:"message"`
}

// Parses the runner inputs and prints only the final result JSON to stdout.
func main() {
	var opts options
	flag.StringVar(&opts.Credentials, "credentials", "", "two-line acceptance credentials file")
	flag.StringVar(&opts.Tests, "tests", "", "resolved private signup tests JSON")
	flag.StringVar(&opts.Fixture, "fixture", "", "persistent instant-account secret-key file")
	flag.StringVar(&opts.ActiveClient, "active-client", "", "private retained client-id file")
	flag.StringVar(&opts.PeerProviderClient, "peer-provider-client", "", "private controlled peer provider client-id file")
	flag.StringVar(&opts.StateDir, "state-dir", "", "private SDK state directory")
	flag.StringVar(&opts.SdkVersion, "sdk-version", "", "SDK build version expected by the daemon")
	flag.StringVar(&opts.AppVersion, "app-version", "", "locally built app/service version")
	flag.StringVar(&opts.ServiceVersion, "service-version", "", "version expected from the running service")
	flag.IntVar(&opts.Repeat, "repeat", 1, "number of complete repetitions")
	flag.BoolVar(&opts.ProviderMode, "peer-provider", false, "run as the controlled same-platform peer provider")
	flag.StringVar(&opts.ProviderReady, "peer-provider-ready", "", "private file that receives the provider client id")
	flag.StringVar(&opts.ProviderStop, "peer-provider-stop", "", "private stop marker watched by the provider")
	flag.StringVar(&opts.ProviderResult, "peer-provider-result", "", "private provider traffic result JSON")
	flag.StringVar(&opts.ProviderEgressInterface, "peer-provider-egress-interface", "", "physical interface name used to keep provider sockets outside the client tunnel")
	flag.UintVar(&opts.ProviderEgressIndex, "peer-provider-egress-index", 0, "physical interface index used to keep provider sockets outside the client tunnel")
	flag.Parse()
	if opts.ProviderMode {
		if err := runPeerProvider(opts); err != nil {
			fmt.Fprintf(os.Stderr, "acceptance peer provider: %v\n", err)
			os.Exit(1)
		}
		return
	}

	result, err := run(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "acceptance: %v\n", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintf(os.Stderr, "acceptance: encode result: %v\n", err)
		os.Exit(1)
	}
}

// Runs the guest lifecycle and data-plane case for every repetition.
func run(opts options) (*acceptanceResult, error) {
	if opts.Credentials == "" || opts.Tests == "" || opts.Fixture == "" || opts.ActiveClient == "" || opts.PeerProviderClient == "" || opts.StateDir == "" || opts.SdkVersion == "" || opts.AppVersion == "" || opts.ServiceVersion == "" || opts.Repeat < 1 {
		return nil, errors.New("credentials, tests, fixture, active-client, peer-provider-client, state-dir, sdk-version, app-version, service-version, and positive repeat are required")
	}
	user, password, err := readCredentials(opts.Credentials)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.StateDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(opts.Fixture), 0o700); err != nil {
		return nil, err
	}
	testsConfig, err := testconfig.LoadJSON(opts.Tests)
	if err != nil {
		return nil, fmt.Errorf("signup fixture: %w", err)
	}
	peerProviderClientId, err := readRetainedClient(opts.PeerProviderClient)
	if err != nil {
		return nil, fmt.Errorf("controlled peer provider: %w", err)
	}

	sdk.Version = opts.SdkVersion
	result := &acceptanceResult{Ok: false, Platform: runtime.GOOS, Repetitions: opts.Repeat}
	fmt.Fprintln(os.Stderr, "acceptance: email, phone, Solana, and Bittensor signup/login/delete lifecycles")
	result.AuthCases = (&authcases.Runner{APIURL: apiUrl, Config: testsConfig}).Run(
		context.Background(), []string{"email", "phone", "solana", "bittensor"},
	)
	for _, authCase := range result.AuthCases {
		fmt.Fprintf(os.Stderr, "acceptance: auth case %s: %s\n", authCase.Case, authCase.Status)
		if authCase.Status != "PASS" {
			return nil, fmt.Errorf("auth case %s: %s", authCase.Case, authCase.Detail)
		}
	}
	for i := 1; i <= opts.Repeat; i++ {
		fmt.Fprintf(os.Stderr, "acceptance: repetition %d/%d: guest login/logout/secret-key login\n", i, opts.Repeat)
		if err := guestCredentialLifecycle(opts.Fixture, filepath.Join(opts.StateDir, "guest")); err != nil {
			return nil, fmt.Errorf("guest credential lifecycle: %w", err)
		}
		var peerResult peerToPeerResult
		before, after, err := runTunnelIteration(opts, user, password, peerProviderClientId, i, &peerResult)
		if err != nil {
			return nil, fmt.Errorf("repetition %d: %w", i, err)
		}
		result.BeforeIps = append(result.BeforeIps, before)
		result.AfterIps = append(result.AfterIps, after)
		result.PeerToPeer = append(result.PeerToPeer, peerResult)
	}
	result.Ok = true
	return result, nil
}

// Runs one password login, provider connection, changed-egress check, and
// unconditional client cleanup through the installed platform service.
func runTunnelIteration(opts options, user, password, peerProviderClientId string, iteration int, peerResult *peerToPeerResult) (beforeIp, afterIp string, returnErr error) {
	iterationState := filepath.Join(opts.StateDir, fmt.Sprintf("iteration-%d", iteration))
	if err := os.MkdirAll(iterationState, 0o700); err != nil {
		return "", "", err
	}
	manager := sdk.NewNetworkSpaceManager(iterationState)
	defer manager.Close()
	networkSpace := manager.UpdateNetworkSpaceValues(
		sdk.NewNetworkSpaceKey("ur.network", "main"),
		&sdk.NetworkSpaceValues{
			Bundled:                  true,
			NetExposeServerIps:       true,
			NetExposeServerHostNames: true,
			LinkHostName:             "ur.io",
			MigrationHostName:        "bringyour.com",
		},
	)
	api := networkSpace.GetApi()

	requestCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	login, err := api.AuthLoginWithPasswordSyncWithContext(requestCtx, &sdk.AuthLoginWithPasswordArgs{
		UserAuth: user,
		Password: password,
	})
	cancel()
	if err != nil {
		return "", "", fmt.Errorf("password login: %w", err)
	}
	if login.Error != nil {
		return "", "", fmt.Errorf("password login: %s", login.Error.Message)
	}
	if login.Network == nil || login.Network.ByJwt == "" {
		return "", "", errors.New("password login returned no network JWT")
	}
	networkJwt := login.Network.ByJwt
	localState := networkSpace.GetAsyncLocalState().GetLocalState()
	if err := localState.SetByJwt(networkJwt); err != nil {
		return "", "", fmt.Errorf("persist network JWT: %w", err)
	}
	api.SetByJwt(networkJwt)

	requestCtx, cancel = context.WithTimeout(context.Background(), 45*time.Second)
	client, err := api.AuthNetworkClientSyncWithContext(requestCtx, &sdk.AuthNetworkClientArgs{
		DeviceDescription: "URnetwork acceptance " + runtime.GOOS,
		DeviceSpec:        runtime.GOOS + "/" + runtime.GOARCH,
	})
	cancel()
	if err != nil {
		return "", "", fmt.Errorf("register network client: %w", err)
	}
	if client.Error != nil {
		return "", "", fmt.Errorf("register network client: %s", client.Error.Message)
	}
	if client.ByClientJwt == "" {
		return "", "", errors.New("register network client returned no client JWT")
	}
	clientJwt := client.ByClientJwt
	clientId, err := jwtStringClaim(clientJwt, "client_id")
	if err != nil {
		return "", "", err
	}
	// Retain the client ID before any tunnel work. A host-side cleanup process
	// can then release it even if this process or its VM/container is killed.
	defer func() {
		clientCleanupErr := removeClient(networkJwt, clientId)
		if clientCleanupErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("release network client: %w", clientCleanupErr))
		} else if err := removeActiveClient(opts.ActiveClient); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("clear retained network client: %w", err))
		}
		if err := localState.Logout(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("clear local SDK session: %w", err))
		}
	}()
	if err := writeActiveClient(opts.ActiveClient, clientId); err != nil {
		return "", "", fmt.Errorf("retain network client for cleanup: %w", err)
	}
	if err := localState.SetByClientJwt(clientJwt); err != nil {
		return "", "", fmt.Errorf("persist client JWT: %w", err)
	}
	instanceId := localState.GetInstanceId()
	if instanceId == nil {
		return "", "", errors.New("client registration did not create an instance ID")
	}

	beforeIp, err = publicIp()
	if err != nil {
		return "", "", fmt.Errorf("physical egress: %w", err)
	}
	fmt.Fprintf(os.Stderr, "acceptance: physical egress %s\n", beforeIp)

	controlCtx, controlCancel := context.WithTimeout(context.Background(), 30*time.Second)
	control, err := newPlatformControl(controlCtx)
	controlCancel()
	if err != nil {
		return "", "", err
	}
	defer func() {
		if err := control.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close platform control: %w", err))
		}
	}()
	tunnelStopped := false
	defer func() {
		if tunnelStopped {
			return
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := control.Stop(stopCtx); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("stop tunnel: %w", err))
		}
	}()

	callCtx, callCancel := context.WithTimeout(context.Background(), 30*time.Second)
	hello, err := control.Hello(callCtx, opts.SdkVersion)
	callCancel()
	if err != nil {
		return "", "", fmt.Errorf("control hello: %w", err)
	}
	if err := validateControlProtocol(runtime.GOOS, hello.ProtocolVersion); err != nil {
		return "", "", err
	}
	if runtime.GOOS == "linux" && hello.SdkVersion != opts.SdkVersion {
		return "", "", fmt.Errorf("SDK version skew: agent %q daemon %q", opts.SdkVersion, hello.SdkVersion)
	}
	if hello.ServiceVersion != opts.ServiceVersion {
		return "", "", fmt.Errorf("service version skew: built %q running %q", opts.ServiceVersion, hello.ServiceVersion)
	}
	callCtx, callCancel = context.WithTimeout(context.Background(), 30*time.Second)
	initial, err := control.Status(callCtx)
	callCancel()
	if err != nil {
		return "", "", fmt.Errorf("initial tunnel status: %w", err)
	}
	if initial.State != "stopped" {
		return "", "", fmt.Errorf("daemon did not start idle: state=%s error=%s", initial.State, initial.Error)
	}

	spaceJson, err := networkSpace.ToJson()
	if err != nil {
		return "", "", err
	}
	config := tunnelConfig{
		ByJwt:             clientJwt,
		NetworkSpaceJson:  spaceJson,
		InstanceId:        instanceId.String(),
		DeviceDescription: "URnetwork acceptance " + runtime.GOOS,
		DeviceSpec:        runtime.GOOS + "/" + runtime.GOARCH,
		AppVersion:        opts.AppVersion,
	}
	rpcMaterial, err := sdk.GenerateDeviceRpcKeyMaterial()
	if err != nil {
		return "", "", err
	}
	config.RpcSessionId, err = mintRpcSessionId(rand.Reader)
	if err != nil {
		return "", "", err
	}
	config.RpcServerPem = rpcMaterial.GetServerPem()
	config.RpcClientCertPem = rpcMaterial.GetClientCertPem()
	config.RpcListenHostPort = "127.0.0.1:12042"

	callCtx, callCancel = context.WithTimeout(context.Background(), 90*time.Second)
	started, err := control.Start(callCtx, config)
	callCancel()
	if err != nil {
		return "", "", fmt.Errorf("start tunnel: %w", err)
	}
	if started.State != "up" {
		started, err = waitForTunnelState(control, "up", 30*time.Second)
		if err != nil {
			return "", "", fmt.Errorf("tunnel did not reach up: state=%s error=%s: %w", started.State, started.Error, err)
		}
	}

	device, err := sdk.NewDeviceRemoteWithDefaults(networkSpace, clientJwt, instanceId)
	if err != nil {
		return "", "", fmt.Errorf("create device remote: %w", err)
	}
	defer device.Close()
	if err := device.SetRpcServer(
		rpcMaterial.GetClientPem(),
		rpcMaterial.GetServerCertPem(),
		config.RpcListenHostPort,
	); err != nil {
		return "", "", fmt.Errorf("configure device RPC: %w", err)
	}
	if err := waitUntil(60*time.Second, func() bool { return device.GetRemoteConnected() }); err != nil {
		return "", "", errors.New("device RPC did not connect")
	}

	controller := device.OpenConnectViewController()
	defer device.CloseConnectViewController(controller)
	controller.ConnectBestAvailable()
	if err := waitUntil(120*time.Second, func() bool {
		return controller.GetConnectionStatus() == sdk.Connected
	}); err != nil {
		return "", "", fmt.Errorf("provider connection: last status %s", controller.GetConnectionStatus())
	}

	afterIp, err = publicIp()
	if err != nil {
		return "", "", fmt.Errorf("network egress: %w", err)
	}
	fmt.Fprintf(os.Stderr, "acceptance: network egress %s\n", afterIp)
	if beforeIp == afterIp {
		return "", "", errors.New("public IP did not change after provider connection")
	}

	if peerResult == nil {
		return "", "", errors.New("peer-to-peer result destination is nil")
	}
	*peerResult, err = runPeerToPeerClient(device, controller, peerProviderClientId)
	if err != nil {
		return "", "", fmt.Errorf("peer-to-peer connection: %w", err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = control.Stop(stopCtx)
	stopCancel()
	if err != nil {
		return "", "", fmt.Errorf("stop tunnel: %w", err)
	}
	tunnelStopped = true
	stopped, err := waitForTunnelState(control, "stopped", 30*time.Second)
	if err != nil {
		return "", "", fmt.Errorf("tunnel did not stop: state=%s error=%s: %w", stopped.State, stopped.Error, err)
	}
	return beforeIp, afterIp, nil
}

// Mints the opaque name that binds one RPC key pair to one service session.
func mintRpcSessionId(random io.Reader) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", fmt.Errorf("mint device RPC session ID: %w", err)
	}
	return hex.EncodeToString(value), nil
}

// Creates or restores one recoverable account, clears the SDK session, and
// proves that its saved secret alone recovers the same network.
func guestCredentialLifecycle(path, stateDir string) error {
	manager := sdk.NewNetworkSpaceManager(stateDir)
	defer manager.Close()
	networkSpace := manager.UpdateNetworkSpaceValues(
		sdk.NewNetworkSpaceKey("ur.network", "main"),
		&sdk.NetworkSpaceValues{
			Bundled:                  true,
			NetExposeServerIps:       true,
			NetExposeServerHostNames: true,
			LinkHostName:             "ur.io",
			MigrationHostName:        "bringyour.com",
		},
	)
	api := networkSpace.GetApi()

	secret, err := readSecret(path)
	var firstJwt string
	if errors.Is(err, os.ErrNotExist) {
		created, err := networkCreate(api)
		if err != nil {
			return err
		}
		if created.Error != nil {
			return errors.New(created.Error.Message)
		}
		if created.Network == nil || created.Network.ByJwt == "" {
			return errors.New("instant account returned no network session")
		}
		firstJwt = created.Network.ByJwt
		secret, err = retainGuestFixture(path, created.Seedphrase, firstJwt, deleteNetwork)
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		firstJwt, err = loginSecret(api, secret)
		if err != nil {
			return err
		}
	}

	return verifyGuestSessionRecovery(api, firstJwt, func() (string, error) {
		return loginSecret(api, secret)
	})
}

// Provides the SDK session operations needed to verify login and logout
// without coupling the deterministic lifecycle tests to the main API.
type guestSession interface {
	SetByJwt(string)
	GetByJwt() string
}

// Installs the initial guest session, logs out, recovers from the saved secret,
// verifies the network identity, and always logs out of the recovered session.
func verifyGuestSessionRecovery(session guestSession, firstJwt string, recoverJwt func() (string, error)) error {
	if err := useGuestSessionAndLogout(session, firstJwt, "initial", nil); err != nil {
		return err
	}
	secondJwt, err := recoverJwt()
	if err != nil {
		return err
	}
	return useGuestSessionAndLogout(session, secondJwt, "recovered", func() error {
		firstNetwork, err := jwtStringClaim(firstJwt, "network_id")
		if err != nil {
			return err
		}
		secondNetwork, err := jwtStringClaim(secondJwt, "network_id")
		if err != nil {
			return err
		}
		if firstNetwork != secondNetwork {
			return errors.New("secret-key login returned a different network")
		}
		return nil
	})
}

// Makes one JWT the active SDK session while the check runs and verifies that
// logout clears it even when the check fails.
func useGuestSessionAndLogout(session guestSession, networkJwt string, phase string, check func() error) (returnErr error) {
	session.SetByJwt(networkJwt)
	defer func() {
		session.SetByJwt("")
		if session.GetByJwt() != "" {
			returnErr = errors.Join(returnErr, fmt.Errorf("SDK did not clear the %s instant-account session on logout", phase))
		}
	}()
	if session.GetByJwt() != networkJwt {
		return fmt.Errorf("SDK did not install the %s instant-account session", phase)
	}
	if check != nil {
		return check()
	}
	return nil
}

// Retains a recoverable credential or deletes the account when it cannot be
// recovered safely after this process exits.
func retainGuestFixture(path, rawSecret, networkJwt string, deleteAccount func(string) error) (string, error) {
	secret := normalizeSecret(rawSecret)
	if secret == "" {
		cleanupErr := deleteAccount(networkJwt)
		return "", errors.Join(
			errors.New("main returned no 24-word secret key for the instant account"),
			cleanupError("delete unrecoverable instant account", cleanupErr),
		)
	}
	if err := writeSecret(path, secret); err != nil {
		cleanupErr := deleteAccount(networkJwt)
		return "", errors.Join(
			fmt.Errorf("persist instant-account secret: %w", err),
			cleanupError("delete instant account after fixture failure", cleanupErr),
		)
	}
	return secret, nil
}

// Adapts the asynchronous SDK create call to a bounded synchronous result.
func networkCreate(api *sdk.Api) (*sdk.NetworkCreateResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	callback, results := connect.NewBlockingApiCallback[*sdk.NetworkCreateResult](ctx)
	api.NetworkCreate(
		&sdk.NetworkCreateArgs{
			NetworkName: fmt.Sprintf("guest-%d", time.Now().UnixNano()),
			GuestMode:   true,
			Terms:       true,
		},
		sdk.NetworkCreateCallback(callback),
	)
	select {
	case result := <-results:
		return result.Result, result.Error
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Adapts secret-key authentication to a bounded network-jwt result.
func loginSecret(api *sdk.Api, secret string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	callback, results := connect.NewBlockingApiCallback[*sdk.AuthLoginResult](ctx)
	api.AuthLogin(
		&sdk.AuthLoginArgs{Seedphrase: secret},
		sdk.AuthLoginCallback(callback),
	)
	var login *sdk.AuthLoginResult
	select {
	case result := <-results:
		if result.Error != nil {
			return "", result.Error
		}
		login = result.Result
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if login == nil {
		return "", errors.New("secret-key login returned no result")
	}
	if login.Error != nil {
		return "", errors.New(login.Error.Message)
	}
	if login.Network == nil || login.Network.ByJwt == "" {
		return "", errors.New("secret-key login returned no network JWT")
	}
	return login.Network.ByJwt, nil
}

// Sends a bounded main-API cleanup request and decodes its JSON result.
func postJson(path string, body any, jwt string, output any) error {
	var requestBody io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		requestBody = bytes.NewReader(payload)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiUrl+path, requestBody)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Version", "1.0.0-native-acceptance")
	if jwt != "" {
		req.Header.Set("Authorization", "Bearer "+jwt)
	}
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	response, err := io.ReadAll(io.LimitReader(res.Body, maxApiResponseBytes))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("%s returned HTTP %d", path, res.StatusCode)
	}
	if err := json.Unmarshal(response, output); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// Wraps a cleanup error only when the cleanup itself failed.
func cleanupError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

// Deletes an instant account that cannot be safely retained as a fixture.
func deleteNetwork(networkJwt string) error {
	var result struct {
		Error *apiError `json:"error"`
	}
	if err := postJson("/auth/network-delete", nil, networkJwt, &result); err != nil {
		return err
	}
	if result.Error != nil {
		return errors.New(result.Error.Message)
	}
	return nil
}

// Releases the registered production client using its network session.
func removeClient(networkJwt, clientId string) error {
	var result struct {
		Error *apiError `json:"error"`
	}
	if err := postJson("/network/remove-client", map[string]any{"client_id": clientId}, networkJwt, &result); err != nil {
		return err
	}
	if result.Error != nil {
		if result.Error.Message == "Client does not exist." {
			return nil
		}
		return errors.New(result.Error.Message)
	}
	return nil
}

// Reads the host's current public address with keep-alive disabled so the
// connected request cannot reuse the physical connection.
func publicIp() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://checkip.amazonaws.com", nil)
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", res.StatusCode)
	}
	value, err := io.ReadAll(io.LimitReader(res.Body, 256))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(value))
	if ip == "" {
		return "", errors.New("empty public IP response")
	}
	return ip, nil
}

// Reads exactly one user line and one password line from the private mount.
func readCredentials(path string) (string, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || strings.TrimSpace(lines[0]) == "" || strings.TrimSpace(lines[1]) == "" {
		return "", "", errors.New("credentials file must have exactly two non-empty lines")
	}
	return strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), nil
}

// Loads and normalizes a private 24-word campaign fixture.
func readSecret(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	secret := normalizeSecret(string(data))
	if secret == "" {
		return "", errors.New("fixture is not a 24-word secret key")
	}
	return secret, nil
}

// Atomically stores a campaign fixture with owner-only permissions.
func writeSecret(path, secret string) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(secret+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return os.Chmod(path, 0o600)
}

// Stores the one non-secret client ID needed for out-of-process cleanup.
func writeActiveClient(path, clientId string) error {
	if clientId == "" {
		return errors.New("client ID is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(clientId+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return os.Chmod(path, 0o600)
}

// Removes a retained client ID after the main API confirms cleanup.
func removeActiveClient(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Canonicalizes whitespace and casing and rejects any non-24-word value.
func normalizeSecret(value string) string {
	words := strings.Fields(strings.ToLower(value))
	if len(words) != 24 {
		return ""
	}
	return strings.Join(words, " ")
}

// Reads one string claim from the trusted HTTPS response without logging the
// bearer token.
func jwtStringClaim(token, name string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("invalid JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", err
	}
	value, ok := claims[name].(string)
	if !ok || value == "" {
		return "", fmt.Errorf("JWT has no %s claim", name)
	}
	return value, nil
}

// Polls a local SDK condition until it succeeds or reaches a hard deadline.
func waitUntil(timeout time.Duration, condition func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return context.DeadlineExceeded
}

// Polls service state with a separate deadline on every control exchange.
func waitForTunnelState(control platformControl, expected string, timeout time.Duration) (tunnelStatus, error) {
	deadline := time.Now().Add(timeout)
	var last tunnelStatus
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		status, err := control.Status(ctx)
		cancel()
		if err != nil {
			return last, err
		}
		last = status
		if status.State == expected {
			return status, nil
		}
		if status.State == "error" {
			return status, errors.New(status.Error)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return last, context.DeadlineExceeded
}

// Returns a string wire value or its protocol default.
func stringValue(value any, fallback string) string {
	if text, ok := value.(string); ok {
		return text
	}
	return fallback
}

// Returns a JSON number or zero when a reply omitted the field.
func numberValue(value any) float64 {
	if number, ok := value.(float64); ok {
		return number
	}
	return 0
}
