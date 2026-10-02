// Package ddblocal runs integration tests against DynamoDB Local.
//
// Use it from a TestMain in files guarded by the `integration` build tag:
//
//	func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }
//
// It starts DynamoDB Local (a Java process) behind a proxy and points the AWS SDK at the proxy through
// AWS_ENDPOINT_URL_DYNAMODB, so production code paths such as handler Boot() methods run unmodified. The proxy
// (proxy.go) counts operations (Counts), measures consumed capacity including index writes (ConsumedCapacity) and lets
// a test inject faults, unprocessed batch items or pauses (AddHook, FailNext, LeaveUnprocessed, PauseFirst).
//
// The table is created from the Terraform definition (LoadTableSchema), so it has exactly production's keys and
// indexes.
//
// DynamoDB Local is found in $DYNAMODB_LOCAL_DIR (default: <repo>/.dynamodb-local, see scripts/setup_test_deps.sh).
// If it cannot be started the test binary fails: integration tests never silently skip.
package ddblocal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	rudolphdynamodb "github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	TableName = "rudolph_integration_test"
	Region    = "us-east-1"
)

// Main starts DynamoDB Local, runs the tests and stops it again. It returns the exit code for os.Exit.
func Main(m *testing.M) int {
	stop, err := start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ddblocal: cannot start DynamoDB Local: %v\n(run scripts/setup_test_deps.sh)\n", err)
		return 1
	}
	defer stop()
	return m.Run()
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func javaBinary() (string, error) {
	if home := os.Getenv("JAVA_HOME"); home != "" {
		return filepath.Join(home, "bin", "java"), nil
	}
	for _, candidate := range []string{"/opt/homebrew/opt/openjdk/bin/java", "/usr/local/opt/openjdk/bin/java"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	path, err := exec.LookPath("java")
	if err != nil {
		return "", errors.New("java not found; set JAVA_HOME")
	}
	// On macOS /usr/bin/java is a stub unless a JDK is installed.
	if err := exec.Command(path, "-version").Run(); err != nil {
		return "", fmt.Errorf("%s is not a working Java runtime: %w", path, err)
	}
	return path, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func start() (func(), error) {
	dir := os.Getenv("DYNAMODB_LOCAL_DIR")
	if dir == "" {
		dir = filepath.Join(repoRoot(), ".dynamodb-local")
	}
	jar := filepath.Join(dir, "DynamoDBLocal.jar")
	if _, err := os.Stat(jar); err != nil {
		return nil, fmt.Errorf("DynamoDB Local jar not found at %s", jar)
	}
	java, err := javaBinary()
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(java,
		"-Djava.library.path="+filepath.Join(dir, "DynamoDBLocal_lib"),
		"-jar", jar, "-inMemory", "-port", fmt.Sprint(port))
	cmd.Dir = dir
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	kill := func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }

	backend := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := waitForPort(fmt.Sprintf("127.0.0.1:%d", port), 30*time.Second); err != nil {
		kill()
		return nil, err
	}

	proxyURL, closeProxy, err := startProxy(backend)
	if err != nil {
		kill()
		return nil, err
	}

	for k, v := range map[string]string{
		"AWS_ENDPOINT_URL_DYNAMODB": proxyURL,
		"AWS_ACCESS_KEY_ID":         "local",
		"AWS_SECRET_ACCESS_KEY":     "local",
		"AWS_REGION":                Region,
		"AWS_EC2_METADATA_DISABLED": "true",
		// Environment read by Rudolph's Lambda handlers in Boot().
		"REGION":        Region,
		"DYNAMODB_NAME": TableName,
	} {
		os.Setenv(k, v)
	}

	return func() { closeProxy(); kill() }, nil
}

func waitForPort(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("DynamoDB Local did not listen on %s within %s", addr, timeout)
}

// The clients are shared by all tests of a process: every SDK client has its own connection pool, and tests that
// create one per call (e.g. per item written) run out of file descriptors.
var (
	clientsMu    sync.Mutex
	sharedRaw    *dynamodb.Client
	sharedClient rudolphdynamodb.DynamoDBClient
)

// Raw returns an AWS SDK client for direct assertions and fixtures.
func Raw(t testing.TB) *dynamodb.Client {
	t.Helper()
	clientsMu.Lock()
	defer clientsMu.Unlock()
	if sharedRaw == nil {
		cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(Region))
		if err != nil {
			t.Fatal(err)
		}
		sharedRaw = dynamodb.NewFromConfig(cfg)
	}
	return sharedRaw
}

// Client returns Rudolph's DynamoDB client wrapper, configured exactly like the Lambdas configure it.
func Client(t testing.TB) rudolphdynamodb.DynamoDBClient {
	t.Helper()
	clientsMu.Lock()
	defer clientsMu.Unlock()
	if sharedClient == nil {
		sharedClient = rudolphdynamodb.GetClient(TableName, Region)
	}
	return sharedClient
}

// Reset recreates an empty table with the schema of deployments/.../modules/store/dynamodb.tf (see LoadTableSchema)
// and clears the operation counters and consumed capacity.
func Reset(t testing.TB) {
	t.Helper()
	ctx := context.Background()
	raw := Raw(t)

	if _, err := raw.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(TableName)}); err != nil {
		var notFound *types.ResourceNotFoundException
		if !errors.As(err, &notFound) {
			t.Fatalf("delete table: %v", err)
		}
	}

	schema, err := LoadTableSchema()
	if err != nil {
		t.Fatalf("table definition: %v", err)
	}
	if _, err := raw.CreateTable(ctx, schema.CreateTableInput(TableName)); err != nil {
		t.Fatalf("create table: %v", err)
	}
	ResetCounts()
}

// Counts returns a snapshot of DynamoDB operations performed since the last ResetCounts.
func Counts() map[string]int {
	stateMu.Lock()
	defer stateMu.Unlock()
	out := make(map[string]int, len(counts))
	for k, v := range counts {
		out[k] = v
	}
	return out
}

// ResetCounts clears the operation counters and the consumed capacity.
func ResetCounts() {
	stateMu.Lock()
	defer stateMu.Unlock()
	counts = map[string]int{}
	capacity = newCapacity()
}
