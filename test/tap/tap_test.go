package tap

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/linkerd/linkerd2/testutil"
)

//////////////////////
///   TEST SETUP   ///
//////////////////////

var TestHelper *testutil.TestHelper

func TestMain(m *testing.M) {
	TestHelper = testutil.NewTestHelper()
	os.Exit(m.Run())
}

type tapEvent struct {
	method     string
	authority  string
	path       string
	httpStatus string
	grpcStatus string
	tls        string
	lineCount  int
}

var (
	expectedT1 = tapEvent{
		method:     "POST",
		authority:  "t1-svc:9090",
		path:       "/buoyantio.bb.TheService/theFunction",
		httpStatus: "200",
		grpcStatus: "OK",
		tls:        "true",
		lineCount:  3,
	}

	expectedT2 = tapEvent{
		method:     "POST",
		authority:  "t2-svc:9090",
		path:       "/buoyantio.bb.TheService/theFunction",
		httpStatus: "200",
		grpcStatus: "Unknown",
		tls:        "true",
		lineCount:  3,
	}

	expectedT3 = tapEvent{
		method:     "POST",
		authority:  "t3-svc:8080",
		path:       "/",
		httpStatus: "200",
		grpcStatus: "",
		tls:        "true",
		lineCount:  3,
	}

	expectedGateway = tapEvent{
		method:     "GET",
		authority:  "gateway-svc:8080",
		path:       "/",
		httpStatus: "500",
		grpcStatus: "",
		tls:        "true",
		lineCount:  3,
	}
)

//////////////////////
/// TEST EXECUTION ///
//////////////////////

func TestCliTap(t *testing.T) {
	out, _, err := TestHelper.LinkerdRun("inject", "--manual", "testdata/tap_application.yaml")
	if err != nil {
		t.Fatalf("linkerd inject command failed\n%s", out)
	}

	prefixedNs := TestHelper.GetTestNamespace("tap-test")
	out, err = TestHelper.KubectlApply(out, prefixedNs)
	if err != nil {
		t.Fatalf("kubectl apply command failed\n%s", out)
	}

	// wait for deployments to start
	for _, deploy := range []string{"t1", "t2", "t3", "gateway"} {
		if err := TestHelper.CheckPods(prefixedNs, deploy, 1); err != nil {
			t.Error(err)
		}

		if err := TestHelper.CheckDeployment(prefixedNs, deploy, 1); err != nil {
			t.Error(fmt.Errorf("Error validating deployment [%s]:\n%s", deploy, err))
		}
	}

	t.Run("tap a deployment", func(t *testing.T) {
		events, err := tap("deploy/t1", "--namespace", prefixedNs)
		if err != nil {
			t.Fatal(err.Error())
		}

		err = validateExpected(events, expectedT1)
		if err != nil {
			t.Fatal(err.Error())
		}
	})

	t.Run("tap a disabled deployment", func(t *testing.T) {
		out, stderr, err := TestHelper.LinkerdRun("tap", "deploy/t4", "--namespace", prefixedNs)
		if out != "" {
			t.Fatalf("Unexpected output: %s", out)
		}
		if err == nil {
			t.Fatal("Expected an error, got none")
		}
		if stderr == "" {
			t.Fatal("Expected an error, got none")
		}
		expectedErr := "Error: all pods found for deployment/t4 have tapping disabled"
		if errs := strings.Split(stderr, "\n"); errs[0] != expectedErr {
			t.Fatalf("Expected [%s], got: %s", expectedErr, errs[0])
		}
	})

	t.Run("tap a service call", func(t *testing.T) {
		events, err := tap("deploy/gateway", "--to", "svc/t2-svc", "--namespace", prefixedNs)
		if err != nil {
			t.Fatal(err.Error())
		}

		err = validateExpected(events, expectedT2)
		if err != nil {
			t.Fatal(err.Error())
		}
	})

	t.Run("tap a pod", func(t *testing.T) {
		deploy := "t3"
		pods, err := TestHelper.GetPodNamesForDeployment(prefixedNs, deploy)
		if err != nil {
			t.Fatalf("Failed to get pods for deployment [%s]\n%s", deploy, err)
		}

		if len(pods) != 1 {
			t.Fatalf("Expected exactly one pod for deployment [%s], got:\n%v", deploy, pods)
		}

		events, err := tap("pod/"+pods[0], "--namespace", prefixedNs)
		if err != nil {
			t.Fatal(err.Error())
		}

		err = validateExpected(events, expectedT3)
		if err != nil {
			t.Fatal(err.Error())
		}
	})

	t.Run("filter tap events by method", func(t *testing.T) {
		events, err := tap("deploy/gateway", "--namespace", prefixedNs, "--method", "GET")
		if err != nil {
			t.Fatal(err.Error())
		}

		err = validateExpected(events, expectedGateway)
		if err != nil {
			t.Fatal(err.Error())
		}
	})

	t.Run("filter tap events by authority", func(t *testing.T) {
		events, err := tap("deploy/gateway", "--namespace", prefixedNs, "--authority", "t1-svc:9090")
		if err != nil {
			t.Fatal(err.Error())
		}

		err = validateExpected(events, expectedT1)
		if err != nil {
			t.Fatal(err.Error())
		}
	})

}

// executes a tap command and converts the command's streaming output into tap
// events using each line's "id" field
func tap(target string, arg ...string) ([]*tapEvent, error) {
	cmd := append([]string{"tap", target}, arg...)
	outputStream, err := TestHelper.LinkerdRunStream(cmd...)
	if err != nil {
		return nil, err
	}
	defer outputStream.Stop()

	outputLines, err := outputStream.ReadUntil(10, 1*time.Minute)
	if err != nil {
		return nil, err
	}

	tapEventByID := make(map[string]*tapEvent)
	for _, line := range outputLines {
		fields := toFieldMap(line)
		obj, ok := tapEventByID[fields["id"]]
		if !ok {
			obj = &tapEvent{}
			tapEventByID[fields["id"]] = obj
		}
		obj.lineCount++
		obj.tls = fields["tls"]

		switch fields["type"] {
		case "req":
			obj.method = fields[":method"]
			obj.authority = fields[":authority"]
			obj.path = fields[":path"]
		case "rsp":
			obj.httpStatus = fields[":status"]
		case "end":
			obj.grpcStatus = fields["grpc-status"]
		}
	}

	output := make([]*tapEvent, 0)
	for _, obj := range tapEventByID {
		if obj.lineCount == 3 { // filter out incomplete events
			output = append(output, obj)
		}
	}

	return output, nil
}

func toFieldMap(line string) map[string]string {
	fields := strings.Fields(line)
	fieldMap := map[string]string{"type": fields[0]}
	for _, field := range fields[1:] {
		parts := strings.SplitN(field, "=", 2)
		fieldMap[parts[0]] = parts[1]
	}
	return fieldMap
}

func validateExpected(events []*tapEvent, expectedEvent tapEvent) error {
	if len(events) == 0 {
		return fmt.Errorf("Expected tap events, got nothing")
	}
	for _, event := range events {
		if *event != expectedEvent {
			return fmt.Errorf("Unexpected tap event [%+v]", *event)
		}
	}
	return nil
}
