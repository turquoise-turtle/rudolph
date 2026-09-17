// Package ddblocal runs integration tests against DynamoDB Local.
//
// Use it from a TestMain in files guarded by the `integration` build tag:
//
//	func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }
//
// It starts DynamoDB Local (a Java process) in front of a request-counting proxy and points the AWS SDK at it
// through AWS_ENDPOINT_URL_DYNAMODB, so production code paths such as handler Boot() methods run unmodified.
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
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

var (
	counterMu sync.Mutex
	counts    = map[string]int{}
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

	proxyURL, closeProxy, err := startCountingProxy(backend)
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

// startCountingProxy counts DynamoDB operations by their X-Amz-Target header (e.g. "PutItem").
func startCountingProxy(backend string) (string, func(), error) {
	target, err := url.Parse(backend)
	if err != nil {
		return "", nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op := r.Header.Get("X-Amz-Target")
		if i := strings.LastIndex(op, "."); i >= 0 {
			op = op[i+1:]
		}
		counterMu.Lock()
		counts[op]++
		counterMu.Unlock()
		proxy.ServeHTTP(w, r)
	})}
	go func() { _ = srv.Serve(l) }()
	return "http://" + l.Addr().String(), func() { _ = srv.Close() }, nil
}

// Raw returns an AWS SDK client for direct assertions and fixtures.
func Raw(t testing.TB) *dynamodb.Client {
	t.Helper()
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(Region))
	if err != nil {
		t.Fatal(err)
	}
	return dynamodb.NewFromConfig(cfg)
}

// Client returns Rudolph's DynamoDB client wrapper, configured exactly like the Lambdas configure it.
func Client(t testing.TB) rudolphdynamodb.DynamoDBClient {
	t.Helper()
	return rudolphdynamodb.GetClient(TableName, Region)
}

// Reset recreates an empty table with the schema from deployments/.../modules/store/dynamodb.tf
// and clears the operation counters.
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

	_, err := raw.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(TableName),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("PK"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("SK"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("DataType"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("SerialNum"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("PK"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("SK"), KeyType: types.KeyTypeRange},
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
			IndexName: aws.String("SerialNum_DataType_MachineID"),
			KeySchema: []types.KeySchemaElement{
				{AttributeName: aws.String("SerialNum"), KeyType: types.KeyTypeHash},
				{AttributeName: aws.String("DataType"), KeyType: types.KeyTypeRange},
			},
			Projection: &types.Projection{
				ProjectionType:   types.ProjectionTypeInclude,
				NonKeyAttributes: []string{"MachineID"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	ResetCounts()
}

// Counts returns a snapshot of DynamoDB operations performed since the last ResetCounts.
func Counts() map[string]int {
	counterMu.Lock()
	defer counterMu.Unlock()
	out := make(map[string]int, len(counts))
	for k, v := range counts {
		out[k] = v
	}
	return out
}

func ResetCounts() {
	counterMu.Lock()
	defer counterMu.Unlock()
	counts = map[string]int{}
}
