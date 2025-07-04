package service_test

import (
	"flag"
	"fmt"
	"math/rand"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	protest "github.com/go-delve/delve/pkg/proc/test"
	"github.com/go-delve/delve/service/debugger"

	"github.com/go-delve/delve/pkg/goversion"
	"github.com/go-delve/delve/pkg/logflags"
	"github.com/go-delve/delve/pkg/proc"
	"github.com/go-delve/delve/service"
	"github.com/go-delve/delve/service/api"
	"github.com/go-delve/delve/service/rpc2"
	"github.com/go-delve/delve/service/rpccommon"
)

var normalLoadConfig = api.LoadConfig{
	FollowPointers:     true,
	MaxVariableRecurse: 1,
	MaxStringLen:       64,
	MaxArrayValues:     64,
	MaxStructFields:    -1,
}

var testBackend, buildMode string

func TestMain(m *testing.M) {
	flag.StringVar(&testBackend, "backend", "", "selects backend")
	flag.StringVar(&buildMode, "test-buildmode", "", "selects build mode")
	var logOutput string
	flag.StringVar(&logOutput, "log-output", "", "configures log output")
	flag.Parse()
	protest.DefaultTestBackend(&testBackend)
	if buildMode != "" && buildMode != "pie" {
		fmt.Fprintf(os.Stderr, "unknown build mode %q", buildMode)
		os.Exit(1)
	}
	logflags.Setup(logOutput != "", logOutput, "")
	protest.RunTestsWithFixtures(m)
}

func withTestClient2(name string, t *testing.T, fn func(c service.Client)) {
	withTestClient2Extended(name, t, 0, [3]string{}, nil, func(c service.Client, fixture protest.Fixture) {
		fn(c)
	})
}

func startServer(name string, buildFlags protest.BuildFlags, t *testing.T, redirects [3]string, args []string) (clientConn net.Conn, fixture protest.Fixture) {
	if testBackend == "rr" {
		protest.MustHaveRecordingAllowed(t)
	}
	listener, clientConn := service.ListenerPipe()
	defer listener.Close()
	if buildMode == "pie" {
		buildFlags |= protest.BuildModePIE
	}
	fixture = protest.BuildFixture(t, name, buildFlags)
	for i := range redirects {
		if redirects[i] != "" {
			redirects[i] = filepath.Join(fixture.BuildDir, redirects[i])
		}
	}
	server := rpccommon.NewServer(&service.Config{
		Listener:    listener,
		ProcessArgs: append([]string{fixture.Path}, args...),
		Debugger: debugger.Config{
			Backend:        testBackend,
			CheckGoVersion: true,
			Packages:       []string{fixture.Source},
			BuildFlags:     "", // build flags can be an empty string here because the only test that uses it, does not set special flags.
			ExecuteKind:    debugger.ExecutingGeneratedFile,
			Stdin:          redirects[0],
			Stdout:         proc.OutputRedirect{Path: redirects[1]},
			Stderr:         proc.OutputRedirect{Path: redirects[2]},
		},
	})
	if err := server.Run(); err != nil {
		t.Fatal(err)
	}
	return clientConn, fixture
}

func withTestClient2Extended(name string, t *testing.T, buildFlags protest.BuildFlags, redirects [3]string, args []string, fn func(c service.Client, fixture protest.Fixture)) {
	clientConn, fixture := startServer(name, buildFlags, t, redirects, args)
	client := rpc2.NewClientFromConn(clientConn)
	defer func() {
		client.Detach(true)
	}()

	fn(client, fixture)
}

func TestRunWithInvalidPath(t *testing.T) {
	if testBackend == "rr" {
		// This test won't work because rr returns an error, after recording, when
		// the recording failed but also when the recording succeeded but the
		// inferior returned an error. Therefore we have to ignore errors from rr.
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("couldn't start listener: %s\n", err)
	}
	defer listener.Close()
	server := rpccommon.NewServer(&service.Config{
		Listener:    listener,
		ProcessArgs: []string{"invalid_path"},
		APIVersion:  2,
		Debugger: debugger.Config{
			Backend:     testBackend,
			ExecuteKind: debugger.ExecutingGeneratedFile,
		},
	})
	if err := server.Run(); err == nil {
		t.Fatal("Expected Run to return error for invalid program path")
	}
}

func TestRestart_afterExit(t *testing.T) {
	withTestClient2("continuetestprog", t, func(c service.Client) {
		origPid := c.ProcessPid()
		state := <-c.Continue()
		if !state.Exited {
			t.Fatal("expected initial process to have exited")
		}
		if _, err := c.Restart(false); err != nil {
			t.Fatal(err)
		}
		if c.ProcessPid() == origPid {
			t.Fatal("did not spawn new process, has same PID")
		}
		state = <-c.Continue()
		if !state.Exited {
			t.Fatalf("expected restarted process to have exited %v", state)
		}
	})
}

func TestRestart_breakpointPreservation(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("continuetestprog", t, func(c service.Client) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.main", Line: 1, Name: "firstbreakpoint", Tracepoint: true})
		assertNoError(err, t, "CreateBreakpoint()")
		stateCh := c.Continue()

		state := <-stateCh
		if state.CurrentThread.Breakpoint.Name != "firstbreakpoint" || !state.CurrentThread.Breakpoint.Tracepoint {
			t.Fatalf("Wrong breakpoint: %#v\n", state.CurrentThread.Breakpoint)
		}
		state = <-stateCh
		if !state.Exited {
			t.Fatal("Did not exit after first tracepoint")
		}

		t.Log("Restart")
		c.Restart(false)
		stateCh = c.Continue()
		state = <-stateCh
		if state.CurrentThread.Breakpoint.Name != "firstbreakpoint" || !state.CurrentThread.Breakpoint.Tracepoint {
			t.Fatalf("Wrong breakpoint (after restart): %#v\n", state.CurrentThread.Breakpoint)
		}
		state = <-stateCh
		if !state.Exited {
			t.Fatal("Did not exit after first tracepoint (after restart)")
		}
	})
}

func TestRestart_duringStop(t *testing.T) {
	withTestClient2("continuetestprog", t, func(c service.Client) {
		origPid := c.ProcessPid()
		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.main", Line: 1})
		if err != nil {
			t.Fatal(err)
		}
		state := <-c.Continue()
		if state.CurrentThread.Breakpoint == nil {
			t.Fatal("did not hit breakpoint")
		}
		if _, err := c.Restart(false); err != nil {
			t.Fatal(err)
		}
		if c.ProcessPid() == origPid {
			t.Fatal("did not spawn new process, has same PID")
		}
		bps, err := c.ListBreakpoints(false)
		if err != nil {
			t.Fatal(err)
		}
		if len(bps) == 0 {
			t.Fatal("breakpoints not preserved")
		}
	})
}

// This source is a slightly modified version of
// _fixtures/testenv.go. The only difference is that
// the name of the environment variable we are trying to
// read is named differently, so we can assert the code
// was actually changed in the test.
const modifiedSource = `package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	x := os.Getenv("SOMEMODIFIEDVAR")
	runtime.Breakpoint()
	fmt.Printf("SOMEMODIFIEDVAR=%s\n", x)
}
`

func TestRestart_rebuild(t *testing.T) {
	// In the original fixture file the env var tested for is SOMEVAR.
	t.Setenv("SOMEVAR", "bah")

	withTestClient2Extended("testenv", t, 0, [3]string{}, nil, func(c service.Client, f protest.Fixture) {
		<-c.Continue()

		var1, err := c.EvalVariable(api.EvalScope{GoroutineID: -1}, "x", normalLoadConfig)
		assertNoError(err, t, "EvalVariable")

		if var1.Value != "bah" {
			t.Fatalf("expected 'bah' got %q", var1.Value)
		}

		fi, err := os.Stat(f.Source)
		assertNoError(err, t, "Stat fixture.Source")

		originalSource, err := os.ReadFile(f.Source)
		assertNoError(err, t, "Reading original source")

		// Ensure we write the original source code back after the test exits.
		defer os.WriteFile(f.Source, originalSource, fi.Mode())

		// Write modified source code to the fixture file.
		err = os.WriteFile(f.Source, []byte(modifiedSource), fi.Mode())
		assertNoError(err, t, "Writing modified source")

		// First set our new env var and ensure later that the
		// modified source code picks it up.
		t.Setenv("SOMEMODIFIEDVAR", "foobar")

		// Restart the program, rebuilding from source.
		_, err = c.Restart(true)
		assertNoError(err, t, "Restart(true)")

		<-c.Continue()

		var1, err = c.EvalVariable(api.EvalScope{GoroutineID: -1}, "x", normalLoadConfig)
		assertNoError(err, t, "EvalVariable")

		if var1.Value != "foobar" {
			t.Fatalf("expected 'foobar' got %q", var1.Value)
		}
	})
}

func TestClientServer_exit(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("continuetestprog", t, func(c service.Client) {
		state, err := c.GetState()
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if e, a := false, state.Exited; e != a {
			t.Fatalf("Expected exited %v, got %v", e, a)
		}
		state = <-c.Continue()
		if state.Err == nil {
			t.Fatalf("Error expected after continue")
		}
		if !state.Exited {
			t.Fatalf("Expected exit after continue: %v", state)
		}
		_, err = c.GetState()
		if err == nil {
			t.Fatal("Expected error on querying state from exited process")
		}
	})
}

func TestClientServer_step(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("testprog", t, func(c service.Client) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.helloworld", Line: -1})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		stateBefore := <-c.Continue()
		if stateBefore.Err != nil {
			t.Fatalf("Unexpected error: %v", stateBefore.Err)
		}

		stateAfter, err := c.Step()
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if before, after := stateBefore.CurrentThread.PC, stateAfter.CurrentThread.PC; before >= after {
			t.Fatalf("Expected %#v to be greater than %#v", after, before)
		}
	})
}

func TestClientServer_stepout(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("testnextprog", t, func(c service.Client) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.helloworld", Line: -1})
		assertNoError(err, t, "CreateBreakpoint()")
		stateBefore := <-c.Continue()
		assertNoError(stateBefore.Err, t, "Continue()")
		if stateBefore.CurrentThread.Line != 13 {
			t.Fatalf("wrong line number %s:%d, expected %d", stateBefore.CurrentThread.File, stateBefore.CurrentThread.Line, 13)
		}
		stateAfter, err := c.StepOut()
		assertNoError(err, t, "StepOut()")
		if stateAfter.CurrentThread.Line != 35 {
			t.Fatalf("wrong line number %s:%d, expected %d", stateAfter.CurrentThread.File, stateAfter.CurrentThread.Line, 13)
		}
	})
}

func testnext2(testcases []nextTest, initialLocation string, t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("testnextprog", t, func(c service.Client) {
		bp, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: initialLocation, Line: -1})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		state := <-c.Continue()
		if state.Err != nil {
			t.Fatalf("Unexpected error: %v", state.Err)
		}

		_, err = c.ClearBreakpoint(bp.ID)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		for _, tc := range testcases {
			if state.CurrentThread.Line != tc.begin {
				t.Fatalf("Program not stopped at correct spot expected %d was %d", tc.begin, state.CurrentThread.Line)
			}

			t.Logf("Next for scenario %#v", tc)
			state, err = c.Next()
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if state.CurrentThread.Line != tc.end {
				t.Fatalf("Program did not continue to correct next location expected %d was %d", tc.end, state.CurrentThread.Line)
			}
		}
	})
}

func TestNextGeneral(t *testing.T) {
	var testcases []nextTest

	ver, _ := goversion.Parse(runtime.Version())

	if ver.Major < 0 || ver.AfterOrEqual(goversion.GoVersion{Major: 1, Minor: 7, Rev: -1}) {
		testcases = []nextTest{
			{17, 19},
			{19, 20},
			{20, 23},
			{23, 24},
			{24, 26},
			{26, 31},
			{31, 23},
			{23, 24},
			{24, 26},
			{26, 31},
			{31, 23},
			{23, 24},
			{24, 26},
			{26, 27},
			{27, 28},
			{28, 34},
		}
	} else {
		testcases = []nextTest{
			{17, 19},
			{19, 20},
			{20, 23},
			{23, 24},
			{24, 26},
			{26, 31},
			{31, 23},
			{23, 24},
			{24, 26},
			{26, 31},
			{31, 23},
			{23, 24},
			{24, 26},
			{26, 27},
			{27, 34},
		}
	}

	testnext2(testcases, "main.testnext", t)
}

func TestNextFunctionReturn(t *testing.T) {
	testcases := []nextTest{
		{13, 14},
		{14, 15},
		{15, 35},
	}
	testnext2(testcases, "main.helloworld", t)
}

func TestClientServer_breakpointInMainThread(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("testprog", t, func(c service.Client) {
		bp, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.helloworld", Line: 1})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		state := <-c.Continue()
		if state.Err != nil {
			t.Fatalf("Unexpected error: %v, state: %#v", state.Err, state)
		}

		pc := state.CurrentThread.PC

		if pc-1 != bp.Addr && pc != bp.Addr {
			f, l := state.CurrentThread.File, state.CurrentThread.Line
			t.Fatalf("Break not respected:\nPC:%#v %s:%d\nFN:%#v \n", pc, f, l, bp.Addr)
		}
	})
}

func TestClientServer_breakpointInSeparateGoroutine(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("testthreads", t, func(c service.Client) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.anotherthread", Line: 1})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		state := <-c.Continue()
		if state.Err != nil {
			t.Fatalf("Unexpected error: %v, state: %#v", state.Err, state)
		}

		f, l := state.CurrentThread.File, state.CurrentThread.Line
		if f != "testthreads.go" && l != 9 {
			t.Fatal("Program did not hit breakpoint")
		}
	})
}

func TestClientServer_breakAtNonexistentPoint(t *testing.T) {
	withTestClient2("testprog", t, func(c service.Client) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "nowhere", Line: 1})
		if err == nil {
			t.Fatal("Should not be able to break at non existent function")
		}
	})
}

func TestClientServer_clearBreakpoint(t *testing.T) {
	withTestClient2("testprog", t, func(c service.Client) {
		bp, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.sleepytime", Line: 1})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if e, a := 1, countBreakpoints(t, c); e != a {
			t.Fatalf("Expected breakpoint count %d, got %d", e, a)
		}

		deleted, err := c.ClearBreakpoint(bp.ID)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if deleted.ID != bp.ID {
			t.Fatalf("Expected deleted breakpoint ID %v, got %v", bp.ID, deleted.ID)
		}

		if e, a := 0, countBreakpoints(t, c); e != a {
			t.Fatalf("Expected breakpoint count %d, got %d", e, a)
		}
	})
}

func TestClientServer_toggleBreakpoint(t *testing.T) {
	withTestClient2("testtoggle", t, func(c service.Client) {
		toggle := func(bp *api.Breakpoint) {
			t.Helper()
			dbp, err := c.ToggleBreakpoint(bp.ID)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if dbp.ID != bp.ID {
				t.Fatalf("The IDs don't match")
			}
		}

		// This one is toggled twice
		bp1, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.lineOne", Tracepoint: true})
		if err != nil {
			t.Fatalf("Unexpected error: %v\n", err)
		}

		toggle(bp1)
		toggle(bp1)

		// This one is toggled once
		bp2, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.lineTwo", Tracepoint: true})
		if err != nil {
			t.Fatalf("Unexpected error: %v\n", err)
		}

		toggle(bp2)

		// This one is never toggled
		bp3, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.lineThree", Tracepoint: true})
		if err != nil {
			t.Fatalf("Unexpected error: %v\n", err)
		}

		if e, a := 3, countBreakpoints(t, c); e != a {
			t.Fatalf("Expected breakpoint count %d, got %d", e, a)
		}

		enableCount := 0
		disabledCount := 0

		contChan := c.Continue()
		for state := range contChan {
			if state.CurrentThread != nil && state.CurrentThread.Breakpoint != nil {
				switch state.CurrentThread.Breakpoint.ID {
				case bp1.ID, bp3.ID:
					enableCount++
				case bp2.ID:
					disabledCount++
				}

				t.Logf("%v", state)
			}
			if state.Exited {
				continue
			}
			if state.Err != nil {
				t.Fatalf("Unexpected error during continue: %v\n", state.Err)
			}
		}

		if enableCount != 2 {
			t.Fatalf("Wrong number of enabled hits: %d\n", enableCount)
		}

		if disabledCount != 0 {
			t.Fatalf("A disabled breakpoint was hit: %d\n", disabledCount)
		}
	})
}

func TestClientServer_toggleAmendedBreakpoint(t *testing.T) {
	withTestClient2("testtoggle", t, func(c service.Client) {
		toggle := func(bp *api.Breakpoint) {
			dbp, err := c.ToggleBreakpoint(bp.ID)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if dbp.ID != bp.ID {
				t.Fatalf("The IDs don't match")
			}
		}

		// This one is toggled twice
		bp, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.lineOne", Tracepoint: true})
		if err != nil {
			t.Fatalf("Unexpected error: %v\n", err)
		}
		bp.Cond = "n == 7"
		assertNoError(c.AmendBreakpoint(bp), t, "AmendBreakpoint() 1")

		// Toggle off.
		toggle(bp)
		// Toggle on.
		toggle(bp)

		amended, err := c.GetBreakpoint(bp.ID)
		if err != nil {
			t.Fatal(err)
		}
		if amended.Cond == "" {
			t.Fatal("breakpoint amendments not preserved after toggle")
		}
	})
}

func TestClientServer_disableHitCondLSSBreakpoint(t *testing.T) {
	withTestClient2("break", t, func(c service.Client) {
		fp := testProgPath(t, "break")
		hitCondBp, err := c.CreateBreakpoint(&api.Breakpoint{
			File:    fp,
			Line:    7,
			HitCond: "< 3",
		})
		assertNoError(err, t, "CreateBreakpoint")
		bp, err := c.CreateBreakpoint(&api.Breakpoint{File: fp, Line: 8})
		assertNoError(err, t, "CreateBreakpoint")

		if len(bp.Addrs) == 0 {
			t.Fatalf("no addresses for breakpoint")
		}

		continueTo := func(ln int, ival string) {
			state := <-c.Continue()
			assertNoError(state.Err, t, fmt.Sprintf("Unexpected error: %v, state: %#v", state.Err, state))

			f, l := state.CurrentThread.File, state.CurrentThread.Line
			if f != fp || l != ln {
				t.Fatalf("Program did not hit breakpoint %s:%d", f, l)
			}

			if ival == "" {
				return
			}

			ivar, err := c.EvalVariable(api.EvalScope{GoroutineID: -1}, "i", normalLoadConfig)
			assertNoError(err, t, "EvalVariable")

			t.Logf("ivar: %s", ivar.SinglelineString())

			if ivar.Value != ival {
				t.Fatalf("Wrong variable value: %s", ivar.Value)
			}
		}

		continueTo(7, "1")
		continueTo(7, "2")
		continueTo(8, "")

		bp, err = c.GetBreakpoint(hitCondBp.ID)
		assertNoError(err, t, "GetBreakpoint()")

		if len(bp.Addrs) != 0 {
			t.Fatalf("Hit condition %s is no longer satisfiable but breakpoint has not been disabled", bp.HitCond)
		}
	})
}

func TestClientServer_disableHitEQLCondBreakpoint(t *testing.T) {
	withTestClient2("break", t, func(c service.Client) {
		fp := testProgPath(t, "break")
		hitCondBp, err := c.CreateBreakpoint(&api.Breakpoint{
			File:    fp,
			Line:    7,
			HitCond: "== 3",
		})
		assertNoError(err, t, "CreateBreakpoint")
		bp, err := c.CreateBreakpoint(&api.Breakpoint{File: fp, Line: 8})
		assertNoError(err, t, "CreateBreakpoint")

		if len(bp.Addrs) == 0 {
			t.Fatalf("no addresses for breakpoint")
		}

		state := <-c.Continue()
		assertNoError(state.Err, t, "Continue")

		f, l := state.CurrentThread.File, state.CurrentThread.Line
		if f != fp || l != 7 {
			t.Fatal("Program did not hit breakpoint")
		}

		ivar, err := c.EvalVariable(api.EvalScope{GoroutineID: -1}, "i", normalLoadConfig)
		assertNoError(err, t, "EvalVariable")

		t.Logf("ivar: %s", ivar.SinglelineString())

		if ivar.Value != "3" {
			t.Fatalf("Wrong variable value: %s", ivar.Value)
		}

		state = <-c.Continue()
		assertNoError(state.Err, t, "Continue")

		if state.CurrentThread.File != fp || state.CurrentThread.Line != 8 {
			t.Fatal("Program did not hit breakpoint")
		}

		bp, err = c.GetBreakpoint(hitCondBp.ID)
		assertNoError(err, t, "GetBreakpoint()")

		if len(bp.Addrs) != 0 {
			t.Fatalf("Hit condition %s is no more satisfiable but breakpoint has not been disabled", bp.HitCond)
		}
	})
}

func TestClientServer_switchThread(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("testnextprog", t, func(c service.Client) {
		// With invalid thread id
		_, err := c.SwitchThread(-1)
		if err == nil {
			t.Fatal("Expected error for invalid thread id")
		}

		_, err = c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.main", Line: 1})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		state := <-c.Continue()
		if state.Err != nil {
			t.Fatalf("Unexpected error: %v, state: %#v", state.Err, state)
		}

		var nt int
		ct := state.CurrentThread.ID
		threads, err := c.ListThreads()
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		for _, th := range threads {
			if th.ID != ct {
				nt = th.ID
				break
			}
		}
		if nt == 0 {
			t.Fatal("could not find thread to switch to")
		}
		// With valid thread id
		state, err = c.SwitchThread(nt)
		if err != nil {
			t.Fatal(err)
		}
		if state.CurrentThread.ID != nt {
			t.Fatal("Did not switch threads")
		}
	})
}

func TestClientServer_infoLocals(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("testnextprog", t, func(c service.Client) {
		fp := testProgPath(t, "testnextprog")
		_, err := c.CreateBreakpoint(&api.Breakpoint{File: fp, Line: 24})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		state := <-c.Continue()
		if state.Err != nil {
			t.Fatalf("Unexpected error: %v, state: %#v", state.Err, state)
		}
		locals, err := c.ListLocalVariables(api.EvalScope{GoroutineID: -1}, normalLoadConfig)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if len(locals) != 3 {
			t.Fatalf("Expected 3 locals, got %d %#v", len(locals), locals)
		}
	})
}

func matchFunctions(t *testing.T, funcs []string, expected []string, depth int) {
	for i := range funcs {
		if funcs[i] != expected[i] {
			t.Fatalf("Function %s  not found in ListFunctions --follow-calls=%d output", expected[i], depth)
		}
	}
}

func TestTraceFollowCallsCommand(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("testtracefns", t, func(c service.Client) {
		depth := 3
		functions, err := c.ListFunctions("main.A", depth)
		assertNoError(err, t, "ListFunctions()")
		expected := []string{"main.A", "main.B", "main.C", "main.D"}
		matchFunctions(t, functions, expected, depth)

		functions, err = c.ListFunctions("main.first", depth)
		assertNoError(err, t, "ListFunctions()")
		expected = []string{"main.first", "main.second"}
		matchFunctions(t, functions, expected, depth)

		depth = 4
		functions, err = c.ListFunctions("main.callme", depth)
		assertNoError(err, t, "ListFunctions()")
		expected = []string{"main.callme", "main.callme2", "main.callmed", "main.callmee"}
		matchFunctions(t, functions, expected, depth)

		depth = 6
		functions, err = c.ListFunctions("main.F0", depth)
		assertNoError(err, t, "ListFunctions()")
		expected = []string{"main.F0", "main.F0.func1", "main.F1", "main.F2", "main.F3", "main.F4", "runtime.deferreturn", "runtime.gopanic", "runtime.gorecover"}
		matchFunctions(t, functions, expected, depth)
	})
}

func TestClientServer_infoArgs(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("testnextprog", t, func(c service.Client) {
		fp := testProgPath(t, "testnextprog")
		_, err := c.CreateBreakpoint(&api.Breakpoint{File: fp, Line: 47})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		state := <-c.Continue()
		if state.Err != nil {
			t.Fatalf("Unexpected error: %v, state: %#v", state.Err, state)
		}
		regs, err := c.ListThreadRegisters(0, false)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if len(regs) == 0 {
			t.Fatal("Expected string showing registers values, got empty string")
		}

		regs, err = c.ListScopeRegisters(api.EvalScope{GoroutineID: -1, Frame: 0}, false)
		assertNoError(err, t, "ListScopeRegisters(-1, 0)")
		if len(regs) == 0 {
			t.Fatal("Expected string showing registers values, got empty string")
		}
		t.Logf("GoroutineID: -1, Frame: 0\n%s", regs.String())

		regs, err = c.ListScopeRegisters(api.EvalScope{GoroutineID: -1, Frame: 1}, false)
		assertNoError(err, t, "ListScopeRegisters(-1, 1)")
		if len(regs) == 0 {
			t.Fatal("Expected string showing registers values, got empty string")
		}
		t.Logf("GoroutineID: -1, Frame: 1\n%s", regs.String())

		locals, err := c.ListFunctionArgs(api.EvalScope{GoroutineID: -1}, normalLoadConfig)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if len(locals) != 2 {
			t.Fatalf("Expected 2 function args, got %d %#v", len(locals), locals)
		}
	})
}

func TestClientServer_traceContinue(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("integrationprog", t, func(c service.Client) {
		fp := testProgPath(t, "integrationprog")
		_, err := c.CreateBreakpoint(&api.Breakpoint{File: fp, Line: 15, Tracepoint: true, Goroutine: true, Stacktrace: 5, Variables: []string{"i"}})
		if err != nil {
			t.Fatalf("Unexpected error: %v\n", err)
		}
		count := 0
		contChan := c.Continue()
		for state := range contChan {
			if state.CurrentThread != nil && state.CurrentThread.Breakpoint != nil {
				count++

				t.Logf("%v", state)

				bpi := state.CurrentThread.BreakpointInfo

				if bpi.Goroutine == nil {
					t.Fatalf("No goroutine information")
				}

				if len(bpi.Stacktrace) == 0 {
					t.Fatalf("No stacktrace\n")
				}

				if len(bpi.Variables) != 1 {
					t.Fatalf("Wrong number of variables returned: %d", len(bpi.Variables))
				}

				if bpi.Variables[0].Name != "i" {
					t.Fatalf("Wrong variable returned %s", bpi.Variables[0].Name)
				}

				t.Logf("Variable i is %v", bpi.Variables[0])

				n, err := strconv.Atoi(bpi.Variables[0].Value)

				if err != nil || n != count-1 {
					t.Fatalf("Wrong variable value %q (%v %d)", bpi.Variables[0].Value, err, count)
				}
			}
			if state.Exited {
				continue
			}
			t.Logf("%v", state)
			if state.Err != nil {
				t.Fatalf("Unexpected error during continue: %v\n", state.Err)
			}
		}

		if count != 3 {
			t.Fatalf("Wrong number of continues hit: %d\n", count)
		}
	})
}

func TestClientServer_traceContinue2(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("integrationprog", t, func(c service.Client) {
		bp1, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.main", Line: 1, Tracepoint: true})
		if err != nil {
			t.Fatalf("Unexpected error: %v\n", err)
		}
		bp2, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.sayhi", Line: 1, Tracepoint: true})
		if err != nil {
			t.Fatalf("Unexpected error: %v\n", err)
		}
		countMain := 0
		countSayhi := 0
		contChan := c.Continue()
		for state := range contChan {
			if state.CurrentThread != nil && state.CurrentThread.Breakpoint != nil {
				switch state.CurrentThread.Breakpoint.ID {
				case bp1.ID:
					countMain++
				case bp2.ID:
					countSayhi++
				}

				t.Logf("%v", state)
			}
			if state.Exited {
				continue
			}
			if state.Err != nil {
				t.Fatalf("Unexpected error during continue: %v\n", state.Err)
			}
		}

		if countMain != 1 {
			t.Fatalf("Wrong number of continues (main.main) hit: %d\n", countMain)
		}

		if countSayhi != 3 {
			t.Fatalf("Wrong number of continues (main.sayhi) hit: %d\n", countSayhi)
		}
	})
}

func TestClientServer_FindLocations(t *testing.T) {
	if runtime.GOARCH == "ppc64le" && buildMode == "pie" {
		t.Skip("pie mode broken on ppc64le")
	}
	withTestClient2("locationsprog", t, func(c service.Client) {
		someFunctionCallAddr := findLocationHelper(t, c, "locationsprog.go:26", false, 1, 0)[0]
		someFunctionLine1 := findLocationHelper(t, c, "locationsprog.go:27", false, 1, 0)[0]
		findLocationHelper(t, c, "anotherFunction:1", false, 1, someFunctionLine1)
		findLocationHelper(t, c, "main.anotherFunction:1", false, 1, someFunctionLine1)
		findLocationHelper(t, c, "anotherFunction", false, 1, someFunctionCallAddr)
		findLocationHelper(t, c, "main.anotherFunction", false, 1, someFunctionCallAddr)
		findLocationHelper(t, c, fmt.Sprintf("*0x%x", someFunctionCallAddr), false, 1, someFunctionCallAddr)
		findLocationHelper(t, c, "sprog.go:26", true, 0, 0)

		findLocationHelper(t, c, "String", true, 0, 0)
		findLocationHelper(t, c, "main.String", true, 0, 0)

		someTypeStringFuncAddr := findLocationHelper(t, c, "locationsprog.go:14", false, 1, 0)[0]
		otherTypeStringFuncAddr := findLocationHelper(t, c, "locationsprog.go:18", false, 1, 0)[0]
		findLocationHelper(t, c, "SomeType.String", false, 1, someTypeStringFuncAddr)
		findLocationHelper(t, c, "(*SomeType).String", false, 1, someTypeStringFuncAddr)
		findLocationHelper(t, c, "main.SomeType.String", false, 1, someTypeStringFuncAddr)
		findLocationHelper(t, c, "main.(*SomeType).String", false, 1, someTypeStringFuncAddr)

		// Issue #275
		readfile := findLocationHelper(t, c, "io/ioutil.ReadFile", false, 1, 0)[0]

		// Issue #296
		findLocationHelper(t, c, "/io/ioutil.ReadFile", false, 1, readfile)
		findLocationHelper(t, c, "ioutil.ReadFile", false, 1, readfile)

		stringAddrs := findLocationHelper(t, c, "/^main.*Type.*String$/", false, 2, 0)

		if otherTypeStringFuncAddr != stringAddrs[0] && otherTypeStringFuncAddr != stringAddrs[1] {
			t.Fatalf("Wrong locations returned for \"/.*Type.*String/\", got: %v expected: %v and %v\n", stringAddrs, someTypeStringFuncAddr, otherTypeStringFuncAddr)
		}

		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.main", Line: 4, Tracepoint: false})
		if err != nil {
			t.Fatalf("CreateBreakpoint(): %v\n", err)
		}

		<-c.Continue()

		locationsprog35Addr := findLocationHelper(t, c, "locationsprog.go:35", false, 1, 0)[0]
		findLocationHelper(t, c, fmt.Sprintf("%s:35", testProgPath(t, "locationsprog")), false, 1, locationsprog35Addr)
		findLocationHelper(t, c, "+1", false, 1, locationsprog35Addr)
		findLocationHelper(t, c, "35", false, 1, locationsprog35Addr)
		findLocationHelper(t, c, "-1", false, 1, findLocationHelper(t, c, "locationsprog.go:33", false, 1, 0)[0])

		findLocationHelper(t, c, `*amap["k"]`, false, 1, findLocationHelper(t, c, `amap["k"]`, false, 1, 0)[0])

		locsNoSubst, _, _ := c.FindLocation(api.EvalScope{GoroutineID: -1}, "_fixtures/locationsprog.go:35", false, nil)
		sep := "/"
		if strings.Contains(locsNoSubst[0].File, "\\") {
			sep = "\\"
		}
		substRules := [][2]string{{strings.Replace(locsNoSubst[0].File, "locationsprog.go", "", 1), strings.Replace(locsNoSubst[0].File, "_fixtures"+sep+"locationsprog.go", "nonexistent", 1)}}
		t.Logf("substitute rules: %q -> %q", substRules[0][0], substRules[0][1])
		locsSubst, _, err := c.FindLocation(api.EvalScope{GoroutineID: -1}, "nonexistent/locationsprog.go:35", false, substRules)
		if err != nil {
			t.Fatalf("FindLocation(locationsprog.go:35) with substitute rules: %v", err)
		}
		t.Logf("FindLocation(\"/nonexistent/path/locationsprog.go:35\") -> %#v", locsSubst)
		if locsNoSubst[0].PC != locsSubst[0].PC {
			t.Fatalf("FindLocation with substitute rules mismatch %#v %#v", locsNoSubst[0], locsSubst[0])
		}
	})

	withTestClient2("testnextdefer", t, func(c service.Client) {
		firstMainLine := findLocationHelper(t, c, "testnextdefer.go:5", false, 1, 0)[0]
		findLocationHelper(t, c, "main.main", false, 1, firstMainLine)
	})

	withTestClient2("stacktraceprog", t, func(c service.Client) {
		stacktracemeAddr := findLocationHelper(t, c, "stacktraceprog.go:4", false, 1, 0)[0]
		findLocationHelper(t, c, "main.stacktraceme", false, 1, stacktracemeAddr)
	})

	withTestClient2Extended("locationsUpperCase", t, 0, [3]string{}, nil, func(c service.Client, fixture protest.Fixture) {
		// Upper case
		findLocationHelper(t, c, "locationsUpperCase.go:6", false, 1, 0)

		// Fully qualified path
		findLocationHelper(t, c, fixture.Source+":6", false, 1, 0)
		bp, err := c.CreateBreakpoint(&api.Breakpoint{File: fixture.Source, Line: 6})
		if err != nil {
			t.Fatalf("Could not set breakpoint in %s: %v\n", fixture.Source, err)
		}
		c.ClearBreakpoint(bp.ID)

		//  Allow `/` or `\` on Windows
		if runtime.GOOS == "windows" {
			findLocationHelper(t, c, filepath.FromSlash(fixture.Source)+":6", false, 1, 0)
			bp, err = c.CreateBreakpoint(&api.Breakpoint{File: filepath.FromSlash(fixture.Source), Line: 6})
			if err != nil {
				t.Fatalf("Could not set breakpoint in %s: %v\n", filepath.FromSlash(fixture.Source), err)
			}
			c.ClearBreakpoint(bp.ID)
		}

		// Case-insensitive on Windows, case-sensitive otherwise
		shouldWrongCaseBeError := true
		numExpectedMatches := 0
		if runtime.GOOS == "windows" {
			shouldWrongCaseBeError = false
			numExpectedMatches = 1
		}
		findLocationHelper(t, c, strings.ToLower(fixture.Source)+":6", shouldWrongCaseBeError, numExpectedMatches, 0)
		bp, err = c.CreateBreakpoint(&api.Breakpoint{File: strings.ToLower(fixture.Source), Line: 6})
		if (err == nil) == shouldWrongCaseBeError {
			t.Fatalf("Could not set breakpoint in %s: %v\n", strings.ToLower(fixture.Source), err)
		}
		c.ClearBreakpoint(bp.ID)
	})

	if goversion.VersionAfterOrEqual(runtime.Version(), 1, 13) {
		withTestClient2("pkgrenames", t, func(c service.Client) {
			someFuncLoc := findLocationHelper(t, c, "github.com/go-delve/delve/_fixtures/internal/dir%2eio.SomeFunction:0", false, 1, 0)[0]
			findLocationHelper(t, c, "dirio.SomeFunction:0", false, 1, someFuncLoc)
		})
	}

	if goversion.VersionAfterOrEqual(runtime.Version(), 1, 18) {
		withTestClient2("locationsprog_generic", t, func(c service.Client) {
			const (
				methodLine = "locationsprog_generic.go:9"
				funcLine   = "locationsprog_generic.go:13"
				funcLine2  = "locationsprog_generic.go:14"
			)
			methodLoc := findLocationHelper2(t, c, methodLine, nil)
			if len(methodLoc.PCs) != 2 {
				// we didn't get both instantiations of the method
				t.Errorf("wrong number of PCs for %s: %#x", methodLine, methodLoc.PCs)
			}

			funcLoc := findLocationHelper2(t, c, funcLine, nil)
			if len(funcLoc.PCs) != 2 {
				// we didn't get both instantiations of the function
				t.Errorf("wrong number of PCs for %s: %#x", funcLine, funcLoc.PCs)
			}

			funcLoc2 := findLocationHelper2(t, c, funcLine2, nil)
			if len(funcLoc2.PCs) != 2 {
				t.Errorf("wrong number of PCs for %s: %#x", funcLine2, funcLoc2.PCs)
			}

			findLocationHelper2(t, c, "main.ParamFunc", funcLoc)

			findLocationHelper2(t, c, "ParamFunc", funcLoc)

			findLocationHelper2(t, c, "main.ParamReceiver.Amethod", methodLoc)
			findLocationHelper2(t, c, "main.Amethod", methodLoc)
			findLocationHelper2(t, c, "ParamReceiver.Amethod", methodLoc)
			findLocationHelper2(t, c, "Amethod", methodLoc)

			findLocationHelper2(t, c, "main.(*ParamReceiver).Amethod", methodLoc)
			findLocationHelper2(t, c, "(*ParamReceiver).Amethod", methodLoc)

			findLocationHelper2(t, c, "main.(*ParamReceiver).Amethod", methodLoc)
			findLocationHelper2(t, c, "(*ParamReceiver).Amethod", methodLoc)

			findLocationHelper2(t, c, "main.ParamFunc:1", funcLoc2)
		})
	}
}

func findLocationHelper2(t *testing.T, c service.Client, loc string, checkLoc *api.Location) *api.Location {
	locs, _, err := c.FindLocation(api.EvalScope{GoroutineID: -1}, loc, false, nil)
	if err != nil {
		t.Fatalf("FindLocation(%q) -> error %v", loc, err)
	}
	t.Logf("FindLocation(%q) → %v\n", loc, locs)
	if len(locs) != 1 {
		t.Logf("Wrong number of locations returned for location %q (got %d expected 1)", loc, len(locs))
	}

	if checkLoc == nil {
		return &locs[0]
	}

	if len(checkLoc.PCs) != len(locs[0].PCs) {
		t.Fatalf("Wrong number of PCs returned (got %#x expected %#x)", locs[0].PCs, checkLoc.PCs)
	}

	for i := range checkLoc.PCs {
		if checkLoc.PCs[i] != locs[0].PCs[i] {
			t.Fatalf("Wrong PCs returned (got %#x expected %#x)", locs[0].PCs, checkLoc.PCs)
		}
	}

	return &locs[0]
}

func TestClientServer_FindLocationsAddr(t *testing.T) {
	withTestClient2("locationsprog2", t, func(c service.Client) {
		<-c.Continue()

		afunction := findLocationHelper(t, c, "main.afunction", false, 1, 0)[0]
		anonfunc := findLocationHelper(t, c, "main.main.func1", false, 1, 0)[0]

		findLocationHelper(t, c, "*fn1", false, 1, afunction)
		findLocationHelper(t, c, "*fn3", false, 1, anonfunc)
	})
}

func TestClientServer_FindLocationsExactMatch(t *testing.T) {
	// if an expression matches multiple functions but one of them is an exact
	// match it should be used anyway.
	// In this example "math/rand.Intn" would normally match "math/rand.Intn"
	// and "math/rand.(*Rand).Intn" but since the first match is exact it
	// should be prioritized.
	withTestClient2("locationsprog3", t, func(c service.Client) {
		<-c.Continue()
		findLocationHelper(t, c, "math/rand.Intn", false, 1, 0)
	})
}

func TestClientServer_EvalVariable(t *testing.T) {
	withTestClient2("testvariables", t, func(c service.Client) {
		state := <-c.Continue()

		if state.Err != nil {
			t.Fatalf("Continue(): %v\n", state.Err)
		}

		var1, err := c.EvalVariable(api.EvalScope{GoroutineID: -1}, "a1", normalLoadConfig)
		assertNoError(err, t, "EvalVariable")

		t.Logf("var1: %s", var1.SinglelineString())

		if var1.Value != "foofoofoofoofoofoo" {
			t.Fatalf("Wrong variable value: %s", var1.Value)
		}
	})
}

func TestClientServer_SetVariable(t *testing.T) {
	withTestClient2("testvariables", t, func(c service.Client) {
		state := <-c.Continue()

		if state.Err != nil {
			t.Fatalf("Continue(): %v\n", state.Err)
		}

		assertNoError(c.SetVariable(api.EvalScope{GoroutineID: -1}, "a2", "8"), t, "SetVariable()")

		a2, err := c.EvalVariable(api.EvalScope{GoroutineID: -1}, "a2", normalLoadConfig)
		if err != nil {
			t.Fatalf("Could not evaluate variable: %v", err)
		}

		t.Logf("a2: %v", a2)

		n, err := strconv.Atoi(a2.Value)

		if err != nil && n != 8 {
			t.Fatalf("Wrong variable value: %v", a2)
		}
	})
}

func TestClientServer_FullStacktrace(t *testing.T) {
	protest.AllowRecording(t)
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		t.Skip("cgo doesn't work on darwin/arm64")
	}
	if runtime.GOARCH == "ppc64le" && buildMode == "pie" {
		t.Skip("pie mode broken on ppc64le")
	}

	lenient := false
	if runtime.GOOS == "windows" {
		lenient = true
	}

	withTestClient2("goroutinestackprog", t, func(c service.Client) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.stacktraceme", Line: -1})
		assertNoError(err, t, "CreateBreakpoint()")
		state := <-c.Continue()
		if state.Err != nil {
			t.Fatalf("Continue(): %v\n", state.Err)
		}

		gs, _, err := c.ListGoroutines(0, 0)
		assertNoError(err, t, "GoroutinesInfo()")
		found := make([]bool, 10)
		for _, g := range gs {
			frames, err := c.Stacktrace(g.ID, 40, 0, &normalLoadConfig)
			assertNoError(err, t, fmt.Sprintf("Stacktrace(%d)", g.ID))
			t.Logf("goroutine %d", g.ID)
			for i, frame := range frames {
				t.Logf("\tframe %d off=%#x bpoff=%#x pc=%#x %s:%d %s", i, frame.FrameOffset, frame.FramePointerOffset, frame.PC, frame.File, frame.Line, frame.Function.Name())
				if frame.Function == nil {
					continue
				}
				if frame.Function.Name() != "main.agoroutine" {
					continue
				}
				for _, arg := range frame.Arguments {
					if arg.Name != "i" {
						continue
					}
					t.Logf("\tvariable i is %+v\n", arg)
					argn, err := strconv.Atoi(arg.Value)
					if err == nil {
						found[argn] = true
					}
				}
			}
		}

		for i := range found {
			if !found[i] {
				if lenient {
					lenient = false
				} else {
					t.Fatalf("Goroutine %d not found", i)
				}
			}
		}

		t.Logf("continue")

		state = <-c.Continue()
		if state.Err != nil {
			t.Fatalf("Continue(): %v\n", state.Err)
		}

		frames, err := c.Stacktrace(-1, 10, 0, &normalLoadConfig)
		assertNoError(err, t, "Stacktrace")

		cur := 3
		for i, frame := range frames {
			t.Logf("\tframe %d off=%#x bpoff=%#x pc=%#x %s:%d %s", i, frame.FrameOffset, frame.FramePointerOffset, frame.PC, frame.File, frame.Line, frame.Function.Name())
			if i == 0 {
				continue
			}
			v := frame.Var("n")
			if v == nil {
				t.Fatalf("Could not find value of variable n in frame %d", i)
			}
			vn, err := strconv.Atoi(v.Value)
			if err != nil || vn != cur {
				t.Fatalf("Expected value %d got %d (error: %v)", cur, vn, err)
			}
			cur--
			if cur < 0 {
				break
			}
		}
	})
}

func assertErrorOrExited(s *api.DebuggerState, err error, t *testing.T, reason string) {
	if err != nil {
		return
	}
	if s != nil && s.Exited {
		return
	}
	t.Fatalf("%s (no error and no exited status)", reason)
}

func TestIssue355(t *testing.T) {
	// After the target process has terminated should return an error but not crash
	protest.AllowRecording(t)
	withTestClient2("continuetestprog", t, func(c service.Client) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.sayhi", Line: -1})
		assertNoError(err, t, "CreateBreakpoint()")
		ch := c.Continue()
		state := <-ch
		tid := state.CurrentThread.ID
		gid := state.SelectedGoroutine.ID
		assertNoError(state.Err, t, "First Continue()")
		ch = c.Continue()
		state = <-ch
		if !state.Exited {
			t.Fatalf("Target did not terminate after second continue")
		}

		ch = c.Continue()
		state = <-ch
		assertError(state.Err, t, "Continue()")

		s, err := c.Next()
		assertErrorOrExited(s, err, t, "Next()")
		s, err = c.Step()
		assertErrorOrExited(s, err, t, "Step()")
		s, err = c.StepInstruction(false)
		assertErrorOrExited(s, err, t, "StepInstruction()")
		s, err = c.SwitchThread(tid)
		assertErrorOrExited(s, err, t, "SwitchThread()")
		s, err = c.SwitchGoroutine(gid)
		assertErrorOrExited(s, err, t, "SwitchGoroutine()")
		s, err = c.Halt()
		assertErrorOrExited(s, err, t, "Halt()")
		_, err = c.ListThreads()
		assertError(err, t, "ListThreads()")
		_, err = c.GetThread(tid)
		assertError(err, t, "GetThread()")
		assertError(c.SetVariable(api.EvalScope{GoroutineID: gid}, "a", "10"), t, "SetVariable()")
		_, err = c.ListLocalVariables(api.EvalScope{GoroutineID: gid}, normalLoadConfig)
		assertError(err, t, "ListLocalVariables()")
		_, err = c.ListFunctionArgs(api.EvalScope{GoroutineID: gid}, normalLoadConfig)
		assertError(err, t, "ListFunctionArgs()")
		_, err = c.ListThreadRegisters(0, false)
		assertError(err, t, "ListThreadRegisters()")
		_, err = c.ListScopeRegisters(api.EvalScope{GoroutineID: gid}, false)
		assertError(err, t, "ListScopeRegisters()")
		_, _, err = c.ListGoroutines(0, 0)
		assertError(err, t, "ListGoroutines()")
		_, err = c.Stacktrace(gid, 10, 0, &normalLoadConfig)
		assertError(err, t, "Stacktrace()")
		_, _, err = c.FindLocation(api.EvalScope{GoroutineID: gid}, "+1", false, nil)
		assertError(err, t, "FindLocation()")
		_, err = c.DisassemblePC(api.EvalScope{GoroutineID: -1}, 0x40100, api.IntelFlavour)
		assertError(err, t, "DisassemblePC()")
	})
}

func TestDisasm(t *testing.T) {
	if runtime.GOARCH == "ppc64le" {
		t.Skip("skipped on ppc64le: broken")
	}
	// Tests that disassembling by PC, range, and current PC all yield similar results
	// Tests that disassembly by current PC will return a disassembly containing the instruction at PC
	// Tests that stepping on a calculated CALL instruction will yield a disassembly that contains the
	// effective destination of the CALL instruction
	withTestClient2("locationsprog2", t, func(c service.Client) {
		ch := c.Continue()
		state := <-ch
		assertNoError(state.Err, t, "Continue()")

		locs, _, err := c.FindLocation(api.EvalScope{GoroutineID: -1}, "main.main", false, nil)
		assertNoError(err, t, "FindLocation()")
		if len(locs) != 1 {
			t.Fatalf("wrong number of locations for main.main: %d", len(locs))
		}
		d1, err := c.DisassemblePC(api.EvalScope{GoroutineID: -1}, locs[0].PC, api.IntelFlavour)
		assertNoError(err, t, "DisassemblePC()")
		if len(d1) < 2 {
			t.Fatalf("wrong size of disassembly: %d", len(d1))
		}

		pcstart := d1[0].Loc.PC
		pcend := d1[len(d1)-1].Loc.PC + uint64(len(d1[len(d1)-1].Bytes))

		// start address should be less than end address
		_, err = c.DisassembleRange(api.EvalScope{GoroutineID: -1}, pcend, pcstart, api.IntelFlavour)
		assertError(err, t, "DisassembleRange()")

		d2, err := c.DisassembleRange(api.EvalScope{GoroutineID: -1}, pcstart, pcend, api.IntelFlavour)
		assertNoError(err, t, "DisassembleRange()")

		if len(d1) != len(d2) {
			t.Logf("d1: %v", d1)
			t.Logf("d2: %v", d2)
			t.Fatal("mismatched length between disassemble pc and disassemble range")
		}

		d3, err := c.DisassemblePC(api.EvalScope{GoroutineID: -1}, state.CurrentThread.PC, api.IntelFlavour)
		assertNoError(err, t, "DisassemblePC() - second call")

		if len(d1) != len(d3) {
			t.Logf("d1: %v", d1)
			t.Logf("d3: %v", d3)
			t.Fatal("mismatched length between the two calls of disassemble pc")
		}

		// look for static call to afunction() on line 29
		found := false
		for i := range d3 {
			if d3[i].Loc.Line == 29 && (strings.HasPrefix(d3[i].Text, "bl") || strings.HasPrefix(d3[i].Text, "jirl") || strings.HasPrefix(d3[i].Text, "call") || strings.HasPrefix(d3[i].Text, "CALL")) && d3[i].DestLoc != nil && d3[i].DestLoc.Function != nil && d3[i].DestLoc.Function.Name() == "main.afunction" {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("Could not find call to main.afunction on line 29")
		}

		haspc := false
		for i := range d3 {
			if d3[i].AtPC {
				haspc = true
				break
			}
		}

		if !haspc {
			t.Logf("d3: %v", d3)
			t.Fatal("PC instruction not found")
		}

		if runtime.GOARCH == "386" && buildMode == "pie" {
			// Skip the rest of the test because on intel 386 with PIE build mode
			// the compiler will insert calls to __x86.get_pc_thunk which do not have DIEs and we can't resolve.
			return
		}

		startinstr := getCurinstr(d3)
		count := 0
		for {
			if count > 20 {
				t.Fatal("too many step instructions executed without finding a call instruction")
			}
			state, err := c.StepInstruction(false)
			assertNoError(err, t, fmt.Sprintf("StepInstruction() %d", count))

			d3, err = c.DisassemblePC(api.EvalScope{GoroutineID: -1}, state.CurrentThread.PC, api.IntelFlavour)
			assertNoError(err, t, fmt.Sprintf("StepInstruction() %d", count))

			curinstr := getCurinstr(d3)

			if curinstr == nil {
				t.Fatalf("Could not find current instruction %d", count)
			}

			if curinstr.Loc.Line != startinstr.Loc.Line {
				t.Fatal("Calling StepInstruction() repeatedly did not find the call instruction")
			}

			if strings.HasPrefix(curinstr.Text, "call") || strings.HasPrefix(curinstr.Text, "CALL") || strings.HasPrefix(curinstr.Text, "bl") || strings.HasPrefix(curinstr.Text, "jirl") {
				t.Logf("call: %v", curinstr)
				if curinstr.DestLoc == nil || curinstr.DestLoc.Function == nil {
					t.Fatalf("Call instruction does not have destination: %v", curinstr)
				}
				if curinstr.DestLoc.Function.Name() != "main.afunction" {
					t.Fatalf("Call instruction destination not main.afunction: %v", curinstr)
				}
				break
			}

			count++
		}
	})
}

func TestNegativeStackDepthBug(t *testing.T) {
	// After the target process has terminated should return an error but not crash
	protest.AllowRecording(t)
	withTestClient2("continuetestprog", t, func(c service.Client) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.sayhi", Line: -1})
		assertNoError(err, t, "CreateBreakpoint()")
		ch := c.Continue()
		state := <-ch
		assertNoError(state.Err, t, "Continue()")
		_, err = c.Stacktrace(-1, -2, 0, &normalLoadConfig)
		assertError(err, t, "Stacktrace()")
	})
}

func TestClientServer_CondBreakpoint(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("parallel_next", t, func(c service.Client) {
		bp, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.sayhi", Line: 1})
		assertNoError(err, t, "CreateBreakpoint()")
		bp.Cond = "n == 7"
		assertNoError(c.AmendBreakpoint(bp), t, "AmendBreakpoint() 1")
		bp, err = c.GetBreakpoint(bp.ID)
		assertNoError(err, t, "GetBreakpoint() 1")
		bp.Variables = append(bp.Variables, "n")
		assertNoError(c.AmendBreakpoint(bp), t, "AmendBreakpoint() 2")
		bp, err = c.GetBreakpoint(bp.ID)
		assertNoError(err, t, "GetBreakpoint() 2")
		if bp.Cond == "" {
			t.Fatalf("No condition set on breakpoint %#v", bp)
		}
		if len(bp.Variables) != 1 {
			t.Fatalf("Wrong number of expressions to evaluate on breakpoint %#v", bp)
		}
		state := <-c.Continue()
		assertNoError(state.Err, t, "Continue()")

		nvar, err := c.EvalVariable(api.EvalScope{GoroutineID: -1}, "n", normalLoadConfig)
		assertNoError(err, t, "EvalVariable()")

		if nvar.SinglelineString() != "7" {
			t.Fatalf("Stopped on wrong goroutine %s\n", nvar.Value)
		}
	})
}

func clientEvalVariable(t *testing.T, c service.Client, expr string) *api.Variable {
	v, err := c.EvalVariable(api.EvalScope{GoroutineID: -1}, expr, normalLoadConfig)
	assertNoError(err, t, fmt.Sprintf("EvalVariable(%s)", expr))
	return v
}

func TestSkipPrologue(t *testing.T) {
	withTestClient2("locationsprog2", t, func(c service.Client) {
		<-c.Continue()

		afunction := findLocationHelper(t, c, "main.afunction", false, 1, 0)[0]
		findLocationHelper(t, c, "*fn1", false, 1, afunction)
		findLocationHelper(t, c, "locationsprog2.go:8", false, 1, afunction)

		afunction0 := clientEvalVariable(t, c, "main.afunction").Addr

		if afunction == afunction0 {
			t.Fatal("Skip prologue failed")
		}
	})
}

func TestSkipPrologue2(t *testing.T) {
	withTestClient2("callme", t, func(c service.Client) {
		callme := findLocationHelper(t, c, "main.callme", false, 1, 0)[0]
		callmeZ := clientEvalVariable(t, c, "main.callme").Addr
		findLocationHelper(t, c, "callme.go:5", false, 1, callme)
		if callme == callmeZ {
			t.Fatal("Skip prologue failed")
		}

		callme2 := findLocationHelper(t, c, "main.callme2", false, 1, 0)[0]
		callme2Z := clientEvalVariable(t, c, "main.callme2").Addr
		findLocationHelper(t, c, "callme.go:12", false, 1, callme2)
		if callme2 == callme2Z {
			t.Fatal("Skip prologue failed")
		}

		callme3 := findLocationHelper(t, c, "main.callme3", false, 1, 0)[0]
		callme3Z := clientEvalVariable(t, c, "main.callme3").Addr
		ver, _ := goversion.Parse(runtime.Version())

		if (ver.Major < 0 || ver.AfterOrEqual(goversion.GoVer18Beta)) && runtime.GOARCH != "386" {
			findLocationHelper(t, c, "callme.go:19", false, 1, callme3)
		} else {
			// callme3 does not have local variables therefore the first line of the
			// function is immediately after the prologue
			// This is only true before go1.8 or on Intel386 where frame pointer chaining
			// introduced a bit of prologue even for functions without local variables
			findLocationHelper(t, c, "callme.go:19", false, 1, callme3Z)
		}
		if callme3 == callme3Z {
			t.Fatal("Skip prologue failed")
		}
	})
}

func TestIssue419(t *testing.T) {
	// Calling service/rpc.(*Client).Halt could cause a crash because both Halt and Continue simultaneously
	// try to read 'runtime.g' and debug/dwarf.Data.Type is not thread safe
	finish := make(chan struct{})
	withTestClient2("issue419", t, func(c service.Client) {
		go func() {
			defer close(finish)
			rand.Seed(time.Now().Unix())
			d := time.Duration(rand.Intn(4) + 1)
			time.Sleep(d * time.Second)
			t.Logf("halt")
			_, err := c.Halt()
			assertNoError(err, t, "RequestManualStop()")
		}()
		statech := c.Continue()
		state := <-statech
		assertNoError(state.Err, t, "Continue()")
		t.Logf("done")
		<-finish
	})
}

func TestTypesCommand(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("testvariables2", t, func(c service.Client) {
		state := <-c.Continue()
		assertNoError(state.Err, t, "Continue()")
		types, err := c.ListTypes("")
		assertNoError(err, t, "ListTypes()")

		if !slices.Contains(types, "main.astruct") {
			t.Fatal("Type astruct not found in ListTypes output")
		}

		types, err = c.ListTypes("^main.astruct$")
		assertNoError(err, t, "ListTypes(\"main.astruct\")")
		if len(types) != 1 {
			t.Fatalf("ListTypes(\"^main.astruct$\") did not filter properly, expected 1 got %d: %v", len(types), types)
		}
	})
}

func TestIssue406(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("issue406", t, func(c service.Client) {
		locs, _, err := c.FindLocation(api.EvalScope{GoroutineID: -1}, "issue406.go:146", false, nil)
		assertNoError(err, t, "FindLocation()")
		_, err = c.CreateBreakpoint(&api.Breakpoint{Addr: locs[0].PC})
		assertNoError(err, t, "CreateBreakpoint()")
		ch := c.Continue()
		state := <-ch
		assertNoError(state.Err, t, "Continue()")
		v, err := c.EvalVariable(api.EvalScope{GoroutineID: -1}, "cfgtree", normalLoadConfig)
		assertNoError(err, t, "EvalVariable()")
		vs := v.MultilineString("", "")
		t.Logf("cfgtree formats to: %s\n", vs)
	})
}

func TestEvalExprName(t *testing.T) {
	withTestClient2("testvariables2", t, func(c service.Client) {
		state := <-c.Continue()
		assertNoError(state.Err, t, "Continue()")

		var1, err := c.EvalVariable(api.EvalScope{GoroutineID: -1}, "i1+1", normalLoadConfig)
		assertNoError(err, t, "EvalVariable")

		const name = "i1+1"

		t.Logf("i1+1 → %#v", var1)

		if var1.Name != name {
			t.Fatalf("Wrong variable name %q, expected %q", var1.Name, name)
		}
	})
}

func TestClientServer_Issue528(t *testing.T) {
	// FindLocation with Receiver.MethodName syntax does not work
	// on remote package names due to a bug in debug/gosym that
	// Was fixed in go 1.7 // Commit that fixes the issue in go:
	// f744717d1924340b8f5e5a385e99078693ad9097

	ver, _ := goversion.Parse(runtime.Version())
	if ver.Major > 0 && !ver.AfterOrEqual(goversion.GoVersion{Major: 1, Minor: 7, Rev: -1}) {
		t.Log("Test skipped")
		return
	}

	withTestClient2("issue528", t, func(c service.Client) {
		findLocationHelper(t, c, "State.Close", false, 1, 0)
	})
}

func TestClientServer_FpRegisters(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("test is valid only on AMD64")
	}
	regtests := []struct{ name, value string }{
		// x87
		{"ST(0)", "0x3fffe666660000000000"},
		{"ST(1)", "0x3fffd9999a0000000000"},
		{"ST(2)", "0x3fffcccccd0000000000"},
		{"ST(3)", "0x3fffc000000000000000"},
		{"ST(4)", "0x3fffb333333333333000"},
		{"ST(5)", "0x3fffa666666666666800"},
		{"ST(6)", "0x3fff9999999999999800"},
		{"ST(7)", "0x3fff8cccccccccccd000"},

		// SSE
		{"XMM0", "0x3ff33333333333333ff199999999999a	v2_int={ 3ff199999999999a 3ff3333333333333 }	v4_int={ 9999999a 3ff19999 33333333 3ff33333 }	v8_int={ 999a 9999 9999 3ff1 3333 3333 3333 3ff3 }	v16_int={ 9a 99 99 99 99 99 f1 3f 33 33 33 33 33 33 f3 3f }"},
		{"XMM1", "0x3ff66666666666663ff4cccccccccccd"},
		{"XMM2", "0x3fe666663fd9999a3fcccccd3fc00000"},
		{"XMM3", "0x3ff199999999999a3ff3333333333333"},
		{"XMM4", "0x3ff4cccccccccccd3ff6666666666666"},
		{"XMM5", "0x3fcccccd3fc000003fe666663fd9999a"},
		{"XMM6", "0x4004cccccccccccc4003333333333334"},
		{"XMM7", "0x40026666666666664002666666666666"},
		{"XMM8", "0x4059999a404ccccd4059999a404ccccd"},

		// AVX 2
		{"XMM11", "0x3ff66666666666663ff4cccccccccccd"},
		{"XMM11", "…[YMM11h] 0x3ff66666666666663ff4cccccccccccd"},

		// AVX 512
		{"XMM12", "0x3ff66666666666663ff4cccccccccccd"},
		{"XMM12", "…[YMM12h] 0x3ff66666666666663ff4cccccccccccd"},
		{"XMM12", "…[ZMM12hl] 0x3ff66666666666663ff4cccccccccccd"},
		{"XMM12", "…[ZMM12hh] 0x3ff66666666666663ff4cccccccccccd"},
	}
	protest.AllowRecording(t)
	withTestClient2Extended("fputest/", t, 0, [3]string{}, nil, func(c service.Client, fixture protest.Fixture) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{File: filepath.Join(fixture.BuildDir, "fputest.go"), Line: 25})
		assertNoError(err, t, "CreateBreakpoint")

		state := <-c.Continue()
		t.Logf("state after continue: %#v", state)

		scope := api.EvalScope{GoroutineID: -1}

		boolvar := func(name string) bool {
			v, err := c.EvalVariable(scope, name, normalLoadConfig)
			if err != nil {
				t.Fatalf("could not read %s variable", name)
			}
			t.Logf("%s variable: %#v", name, v)
			return v.Value != "false"
		}

		avx2 := boolvar("avx2")
		avx512 := boolvar("avx512")

		if runtime.GOOS == "windows" {
			// not supported
			avx2 = false
			avx512 = false
		}

		state = <-c.Continue()
		t.Logf("state after continue: %#v", state)

		regs, err := c.ListThreadRegisters(0, true)
		assertNoError(err, t, "ListThreadRegisters()")

		for _, regtest := range regtests {
			if regtest.name == "XMM11" && !avx2 {
				continue
			}
			if regtest.name == "XMM12" && (!avx512 || testBackend == "rr") {
				continue
			}
			found := false
			for _, reg := range regs {
				if reg.Name == regtest.name {
					found = true
					if strings.HasPrefix(regtest.value, "…") {
						if !strings.Contains(reg.Value, regtest.value[len("…"):]) {
							t.Fatalf("register %s expected to contain %q got %q", reg.Name, regtest.value, reg.Value)
						}
					} else {
						if !strings.HasPrefix(reg.Value, regtest.value) {
							t.Fatalf("register %s expected %q got %q", reg.Name, regtest.value, reg.Value)
						}
					}
				}
			}
			if !found {
				t.Fatalf("register %s not found: %v", regtest.name, regs)
			}
		}

		// Test register expressions

		for _, tc := range []struct{ expr, tgt string }{
			{"XMM1[:32]", `"cdccccccccccf43f666666666666f63f"`},
			{"_XMM1[:32]", `"cdccccccccccf43f666666666666f63f"`},
			{"__XMM1[:32]", `"cdccccccccccf43f666666666666f63f"`},
			{"XMM1.int8[0]", `-51`},
			{"XMM1.uint16[0]", `52429`},
			{"XMM1.float32[0]", `-107374184`},
			{"XMM1.float64[0]", `1.3`},
			{"RAX.uint8[0]", "42"},
		} {
			v, err := c.EvalVariable(scope, tc.expr, normalLoadConfig)
			if err != nil {
				t.Fatalf("could not evalue expression %s: %v", tc.expr, err)
			}
			out := v.SinglelineString()

			if out != tc.tgt {
				t.Fatalf("for %q expected %q got %q\n", tc.expr, tc.tgt, out)
			}
		}
	})
}

func TestClientServer_RestartBreakpointPosition(t *testing.T) {
	protest.AllowRecording(t)
	if buildMode == "pie" || (runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("not meaningful in PIE mode")
	}
	withTestClient2("locationsprog2", t, func(c service.Client) {
		bpBefore, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.afunction", Line: -1, Tracepoint: true, Name: "this"})
		addrBefore := bpBefore.Addr
		t.Logf("%x\n", bpBefore.Addr)
		assertNoError(err, t, "CreateBreakpoint")
		stateCh := c.Continue()
		for range stateCh {
		}
		_, err = c.Halt()
		assertNoError(err, t, "Halt")
		_, err = c.Restart(false)
		assertNoError(err, t, "Restart")
		bps, err := c.ListBreakpoints(false)
		assertNoError(err, t, "ListBreakpoints")
		for _, bp := range bps {
			if bp.Name == bpBefore.Name {
				if bp.Addr != addrBefore {
					t.Fatalf("Address changed after restart: %x %x", bp.Addr, addrBefore)
				}
				t.Logf("%x %x\n", bp.Addr, addrBefore)
			}
		}
	})
}

func TestClientServer_SelectedGoroutineLoc(t *testing.T) {
	// CurrentLocation of SelectedGoroutine should reflect what's happening on
	// the thread running the goroutine, not the position the goroutine was in
	// the last time it was parked.
	protest.AllowRecording(t)
	withTestClient2("testprog", t, func(c service.Client) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.main", Line: -11})
		assertNoError(err, t, "CreateBreakpoint")

		s := <-c.Continue()
		assertNoError(s.Err, t, "Continue")

		gloc := s.SelectedGoroutine.CurrentLoc

		if gloc.PC != s.CurrentThread.PC {
			t.Errorf("mismatched PC %#x %#x", gloc.PC, s.CurrentThread.PC)
		}

		if gloc.File != s.CurrentThread.File || gloc.Line != s.CurrentThread.Line {
			t.Errorf("mismatched file:lineno: %s:%d %s:%d", gloc.File, gloc.Line, s.CurrentThread.File, s.CurrentThread.Line)
		}
	})
}

func TestClientServer_ReverseContinue(t *testing.T) {
	protest.AllowRecording(t)
	if testBackend != "rr" {
		t.Skip("backend is not rr")
	}
	withTestClient2("continuetestprog", t, func(c service.Client) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.main", Line: -1})
		assertNoError(err, t, "CreateBreakpoint(main.main)")
		_, err = c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.sayhi", Line: -1})
		assertNoError(err, t, "CreateBreakpoint(main.sayhi)")

		state := <-c.Continue()
		assertNoError(state.Err, t, "first continue")
		mainPC := state.CurrentThread.PC
		t.Logf("after first continue %#x", mainPC)

		state = <-c.Continue()
		assertNoError(state.Err, t, "second continue")
		sayhiPC := state.CurrentThread.PC
		t.Logf("after second continue %#x", sayhiPC)

		if mainPC == sayhiPC {
			t.Fatalf("expected different PC after second PC (%#x)", mainPC)
		}

		state = <-c.Rewind()
		assertNoError(state.Err, t, "rewind")

		if mainPC != state.CurrentThread.PC {
			t.Fatalf("Expected rewind to go back to the first breakpoint: %#x", state.CurrentThread.PC)
		}
	})
}

func TestClientServer_collectBreakpointInfoOnNext(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("testnextprog", t, func(c service.Client) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{
			Addr:       findLocationHelper(t, c, "testnextprog.go:23", false, 1, 0)[0],
			Variables:  []string{"j"},
			LoadLocals: &normalLoadConfig})
		assertNoError(err, t, "CreateBreakpoint()")
		_, err = c.CreateBreakpoint(&api.Breakpoint{
			Addr:       findLocationHelper(t, c, "testnextprog.go:24", false, 1, 0)[0],
			Variables:  []string{"j"},
			LoadLocals: &normalLoadConfig})
		assertNoError(err, t, "CreateBreakpoint()")

		stateBefore := <-c.Continue()
		assertNoError(stateBefore.Err, t, "Continue()")
		if stateBefore.CurrentThread.Line != 23 {
			t.Fatalf("wrong line number %s:%d, expected %d", stateBefore.CurrentThread.File, stateBefore.CurrentThread.Line, 23)
		}
		if bi := stateBefore.CurrentThread.BreakpointInfo; bi == nil || len(bi.Variables) != 1 {
			t.Fatalf("bad breakpoint info %v", bi)
		}

		stateAfter, err := c.Next()
		assertNoError(err, t, "Next()")
		if stateAfter.CurrentThread.Line != 24 {
			t.Fatalf("wrong line number %s:%d, expected %d", stateAfter.CurrentThread.File, stateAfter.CurrentThread.Line, 24)
		}
		if bi := stateAfter.CurrentThread.BreakpointInfo; bi == nil || len(bi.Variables) != 1 {
			t.Fatalf("bad breakpoint info %v", bi)
		}
	})
}

func TestClientServer_collectBreakpointInfoError(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("testnextprog", t, func(c service.Client) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{
			Addr:       findLocationHelper(t, c, "testnextprog.go:23", false, 1, 0)[0],
			Variables:  []string{"nonexistentvariable", "j"},
			LoadLocals: &normalLoadConfig})
		assertNoError(err, t, "CreateBreakpoint()")
		state := <-c.Continue()
		assertNoError(state.Err, t, "Continue()")
	})
}

func TestClientServerConsistentExit(t *testing.T) {
	// This test is useful because it ensures that Next and Continue operations both
	// exit with the same exit status and details when the target application terminates.
	// Other program execution API calls should also behave in the same way.
	// An error should be present in state.Err.
	withTestClient2("pr1055", t, func(c service.Client) {
		fp := testProgPath(t, "pr1055")
		_, err := c.CreateBreakpoint(&api.Breakpoint{File: fp, Line: 12})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		state := <-c.Continue()
		if state.Err != nil {
			t.Fatalf("Unexpected error: %v, state: %#v", state.Err, state)
		}
		state, err = c.Next()
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if !state.Exited {
			t.Fatal("Process state is not exited")
		}
		if state.ExitStatus != 2 {
			t.Fatalf("Process exit status is not 2, got: %v", state.ExitStatus)
		}

		// Ensure future commands also return the correct exit status.
		// Previously there was a bug where the command which prompted the
		// process to exit (continue, next, etc...) would return the current
		// exit status but subsequent commands would return an incorrect exit
		// status of 0. To test this we simply repeat the 'next' command and
		// ensure we get the correct response again.
		state, err = c.Next()
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if !state.Exited {
			t.Fatal("Second process state is not exited")
		}
		if state.ExitStatus != 2 {
			t.Fatalf("Second process exit status is not 2, got: %v", state.ExitStatus)
		}
	})
}

func TestClientServer_StepOutReturn(t *testing.T) {
	ver, _ := goversion.Parse(runtime.Version())
	if ver.Major >= 0 && !ver.AfterOrEqual(goversion.GoVersion{Major: 1, Minor: 10, Rev: -1}) {
		t.Skip("return variables aren't marked on 1.9 or earlier")
	}
	withTestClient2("stepoutret", t, func(c service.Client) {
		c.SetReturnValuesLoadConfig(&normalLoadConfig)
		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.stepout", Line: -1})
		assertNoError(err, t, "CreateBreakpoint()")
		stateBefore := <-c.Continue()
		assertNoError(stateBefore.Err, t, "Continue()")
		stateAfter, err := c.StepOut()
		assertNoError(err, t, "StepOut")
		ret := stateAfter.CurrentThread.ReturnValues

		if len(ret) != 2 {
			t.Fatalf("wrong number of return values %v", ret)
		}

		stridx := 0
		numidx := 1

		if ret[stridx].Name != "str" {
			t.Fatalf("(str) bad return value name %s", ret[stridx].Name)
		}
		if ret[stridx].Kind != reflect.String {
			t.Fatalf("(str) bad return value kind %v", ret[stridx].Kind)
		}
		if ret[stridx].Value != "return 47" {
			t.Fatalf("(str) bad return value %q", ret[stridx].Value)
		}

		if ret[numidx].Name != "num" {
			t.Fatalf("(num) bad return value name %s", ret[numidx].Name)
		}
		if ret[numidx].Kind != reflect.Int {
			t.Fatalf("(num) bad return value kind %v", ret[numidx].Kind)
		}
		if ret[numidx].Value != "48" {
			t.Fatalf("(num) bad return value %s", ret[numidx].Value)
		}
	})
}

func TestAcceptMulticlient(t *testing.T) {
	if testBackend == "rr" {
		t.Skip("recording not allowed for TestAcceptMulticlient")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("couldn't start listener: %s\n", err)
	}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer listener.Close()
		disconnectChan := make(chan struct{})
		server := rpccommon.NewServer(&service.Config{
			Listener:       listener,
			ProcessArgs:    []string{protest.BuildFixture(t, "testvariables2", 0).Path},
			AcceptMulti:    true,
			DisconnectChan: disconnectChan,
			Debugger: debugger.Config{
				Backend:     testBackend,
				ExecuteKind: debugger.ExecutingGeneratedTest,
			},
		})
		if err := server.Run(); err != nil {
			panic(err)
		}
		<-disconnectChan
		server.Stop()
	}()
	client1 := rpc2.NewClient(listener.Addr().String())
	client1.Disconnect(false)

	client2 := rpc2.NewClient(listener.Addr().String())
	state := <-client2.Continue()
	if state.CurrentThread.Function.Name() != "main.main" {
		t.Fatalf("bad state after continue: %v\n", state)
	}
	client2.Detach(true)
	<-serverDone
}

func TestForceStopWhileContinue(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("couldn't start listener: %s\n", err)
	}
	serverStopped := make(chan struct{})
	disconnectChan := make(chan struct{})
	go func() {
		defer close(serverStopped)
		defer listener.Close()
		server := rpccommon.NewServer(&service.Config{
			Listener:       listener,
			ProcessArgs:    []string{protest.BuildFixture(t, "http_server", protest.AllNonOptimized).Path},
			AcceptMulti:    true,
			DisconnectChan: disconnectChan,
			Debugger: debugger.Config{
				Backend: "default",
			},
		})
		if err := server.Run(); err != nil {
			panic(err)
		}
		<-disconnectChan
		server.Stop()
	}()

	client := rpc2.NewClient(listener.Addr().String())
	client.Disconnect(true /*continue*/)
	time.Sleep(10 * time.Millisecond) // give server time to start running
	close(disconnectChan)             // stop the server
	<-serverStopped                   // Stop() didn't block on detach because we halted first
}

func TestClientServerFunctionCall(t *testing.T) {
	if buildMode == "pie" && runtime.GOARCH == "ppc64le" {
		t.Skip("Debug function call Test broken in PIE mode")
	}

	protest.MustSupportFunctionCalls(t, testBackend)
	withTestClient2("fncall", t, func(c service.Client) {
		c.SetReturnValuesLoadConfig(&normalLoadConfig)
		state := <-c.Continue()
		assertNoError(state.Err, t, "Continue()")
		beforeCallFn := state.CurrentThread.Function.Name()
		state, err := c.Call(-1, "call1(one, two)", false)
		assertNoError(err, t, "Call()")
		t.Logf("returned to %q", state.CurrentThread.Function.Name())
		if state.CurrentThread.Function.Name() != beforeCallFn {
			t.Fatalf("did not return to the calling function %q %q", beforeCallFn, state.CurrentThread.Function.Name())
		}
		if state.CurrentThread.ReturnValues == nil {
			t.Fatal("no return values on return from call")
		}
		t.Logf("Return values %v", state.CurrentThread.ReturnValues)
		if len(state.CurrentThread.ReturnValues) != 1 {
			t.Fatal("not enough return values")
		}
		if state.CurrentThread.ReturnValues[0].Value != "3" {
			t.Fatalf("wrong return value %s", state.CurrentThread.ReturnValues[0].Value)
		}
		state = <-c.Continue()
		if !state.Exited {
			t.Fatalf("expected process to exit after call %v", state.CurrentThread)
		}
	})
}

func TestClientServerFunctionCallPanic(t *testing.T) {
	if buildMode == "pie" && runtime.GOARCH == "ppc64le" {
		t.Skip("Debug function call Test broken in PIE mode")
	}
	protest.MustSupportFunctionCalls(t, testBackend)
	withTestClient2("fncall", t, func(c service.Client) {
		c.SetReturnValuesLoadConfig(&normalLoadConfig)
		state := <-c.Continue()
		assertNoError(state.Err, t, "Continue()")
		state, err := c.Call(-1, "callpanic()", false)
		assertNoError(err, t, "Call()")
		t.Logf("at: %s:%d", state.CurrentThread.File, state.CurrentThread.Line)
		if state.CurrentThread.ReturnValues == nil {
			t.Fatal("no return values on return from call")
		}
		t.Logf("Return values %v", state.CurrentThread.ReturnValues)
		if len(state.CurrentThread.ReturnValues) != 1 {
			t.Fatal("not enough return values")
		}
		if state.CurrentThread.ReturnValues[0].Name != "~panic" {
			t.Fatal("not a panic")
		}
		if state.CurrentThread.ReturnValues[0].Children[0].Value != "callpanic panicked" {
			t.Fatalf("wrong panic value %s", state.CurrentThread.ReturnValues[0].Children[0].Value)
		}
	})
}

func TestClientServerFunctionCallStacktrace(t *testing.T) {
	if goversion.VersionAfterOrEqual(runtime.Version(), 1, 15) {
		t.Skip("Go 1.15 executes function calls in a different goroutine so the stack trace will not contain main.main or runtime.main")
	}
	protest.MustSupportFunctionCalls(t, testBackend)
	withTestClient2("fncall", t, func(c service.Client) {
		c.SetReturnValuesLoadConfig(&api.LoadConfig{FollowPointers: false, MaxStringLen: 2048})
		state := <-c.Continue()
		assertNoError(state.Err, t, "Continue()")
		state, err := c.Call(-1, "callstacktrace()", false)
		assertNoError(err, t, "Call()")
		t.Logf("at: %s:%d", state.CurrentThread.File, state.CurrentThread.Line)
		if state.CurrentThread.ReturnValues == nil {
			t.Fatal("no return values on return from call")
		}
		if len(state.CurrentThread.ReturnValues) != 1 || state.CurrentThread.ReturnValues[0].Kind != reflect.String {
			t.Fatal("not enough return values")
		}
		st := state.CurrentThread.ReturnValues[0].Value
		t.Logf("Returned stacktrace:\n%s", st)

		if !strings.Contains(st, "main.callstacktrace") || !strings.Contains(st, "main.main") || !strings.Contains(st, "runtime.main") {
			t.Fatal("bad stacktrace returned")
		}
	})
}

func TestAncestors(t *testing.T) {
	if !goversion.VersionAfterOrEqual(runtime.Version(), 1, 11) {
		t.Skip("not supported on Go <= 1.10")
	}
	t.Setenv("GODEBUG", "tracebackancestors=100")
	withTestClient2("testnextprog", t, func(c service.Client) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.testgoroutine", Line: -1})
		assertNoError(err, t, "CreateBreakpoint")
		state := <-c.Continue()
		assertNoError(state.Err, t, "Continue()")
		ancestors, err := c.Ancestors(-1, 1000, 1000)
		assertNoError(err, t, "Ancestors")
		t.Logf("ancestors: %#v\n", ancestors)
		if len(ancestors) != 1 {
			t.Fatalf("expected only one ancestor got %d", len(ancestors))
		}

		mainFound := false
		for _, ancestor := range ancestors {
			for _, frame := range ancestor.Stack {
				if frame.Function.Name() == "main.main" {
					mainFound = true
				}
			}
		}
		if !mainFound {
			t.Fatal("function main.main not found in any ancestor")
		}
	})
}

type brokenRPCClient struct {
	client *rpc.Client
}

func (c *brokenRPCClient) Detach(kill bool) error {
	defer c.client.Close()
	out := new(rpc2.DetachOut)
	return c.call("Detach", rpc2.DetachIn{Kill: kill}, out)
}

func (c *brokenRPCClient) call(method string, args, reply interface{}) error {
	return c.client.Call("RPCServer."+method, args, reply)
}

func TestUnknownMethodCall(t *testing.T) {
	clientConn, _ := startServer("continuetestprog", 0, t, [3]string{}, nil)
	client := &brokenRPCClient{jsonrpc.NewClient(clientConn)}
	client.call("SetApiVersion", api.SetAPIVersionIn{APIVersion: 2}, &api.SetAPIVersionOut{})
	defer client.Detach(true)
	var out int
	err := client.call("NonexistentRPCCall", nil, &out)
	assertError(err, t, "call()")
	if !strings.HasPrefix(err.Error(), "unknown method: ") {
		t.Errorf("wrong error message: %v", err)
	}
}

func TestIssue1703(t *testing.T) {
	// Calling Disassemble when there is no current goroutine should work.
	withTestClient2("testnextprog", t, func(c service.Client) {
		locs, _, err := c.FindLocation(api.EvalScope{GoroutineID: -1}, "main.main", true, nil)
		assertNoError(err, t, "FindLocation")
		t.Logf("FindLocation: %#v", locs)
		text, err := c.DisassemblePC(api.EvalScope{GoroutineID: -1}, locs[0].PC, api.IntelFlavour)
		assertNoError(err, t, "DisassemblePC")
		t.Logf("text: %#v\n", text)
	})
}

func TestRerecord(t *testing.T) {
	protest.AllowRecording(t)
	if testBackend != "rr" {
		t.Skip("only valid for recorded targets")
	}
	withTestClient2("testrerecord", t, func(c service.Client) {
		fp := testProgPath(t, "testrerecord")
		_, err := c.CreateBreakpoint(&api.Breakpoint{File: fp, Line: 10})
		assertNoError(err, t, "CreateBreakpoint")

		gett := func() int {
			state := <-c.Continue()
			if state.Err != nil {
				t.Fatalf("Unexpected error: %v, state: %#v", state.Err, state)
			}

			vart, err := c.EvalVariable(api.EvalScope{GoroutineID: -1}, "t", normalLoadConfig)
			assertNoError(err, t, "EvalVariable")
			if vart.Unreadable != "" {
				t.Fatalf("Could not read variable 't': %s\n", vart.Unreadable)
			}

			t.Logf("Value of t is %s\n", vart.Value)

			vartval, err := strconv.Atoi(vart.Value)
			assertNoError(err, t, "Parsing value of variable t")
			return vartval
		}

		t0 := gett()

		_, err = c.RestartFrom(false, "", false, nil, [3]string{}, false)
		assertNoError(err, t, "First restart")
		t1 := gett()

		if t0 != t1 {
			t.Fatalf("Expected same value for t after restarting (without rerecording) %d %d", t0, t1)
		}

		time.Sleep(2 * time.Second) // make sure that we're not running inside the same second

		_, err = c.RestartFrom(true, "", false, nil, [3]string{}, false)
		assertNoError(err, t, "Second restart")
		t2 := gett()

		if t0 == t2 {
			t.Fatalf("Expected new value for t after restarting (with rerecording) %d %d", t0, t2)
		}
	})
}

func TestIssue1787(t *testing.T) {
	// Calling FunctionReturnLocations without a selected goroutine should
	// work.
	withTestClient2("testnextprog", t, func(c service.Client) {
		if c, _ := c.(*rpc2.RPCClient); c != nil {
			c.FunctionReturnLocations("main.main")
		}
	})
}

func TestDoubleCreateBreakpoint(t *testing.T) {
	withTestClient2("testnextprog", t, func(c service.Client) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.main", Line: 1, Name: "firstbreakpoint", Tracepoint: true})
		assertNoError(err, t, "CreateBreakpoint 1")

		bps, err := c.ListBreakpoints(false)
		assertNoError(err, t, "ListBreakpoints 1")

		t.Logf("breakpoints before second call:")
		for _, bp := range bps {
			t.Logf("\t%v", bp)
		}

		numBreakpoints := len(bps)

		_, err = c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.main", Line: 1, Name: "secondbreakpoint", Tracepoint: true})
		assertError(err, t, "CreateBreakpoint 2") // breakpoint exists

		bps, err = c.ListBreakpoints(false)
		assertNoError(err, t, "ListBreakpoints 2")

		t.Logf("breakpoints after second call:")
		for _, bp := range bps {
			t.Logf("\t%v", bp)
		}

		if len(bps) != numBreakpoints {
			t.Errorf("wrong number of breakpoints, got %d expected %d", len(bps), numBreakpoints)
		}
	})
}

func TestStopRecording(t *testing.T) {
	protest.AllowRecording(t)
	if testBackend != "rr" {
		t.Skip("only for rr backend")
	}
	withTestClient2("sleep", t, func(c service.Client) {
		time.Sleep(time.Second)
		c.StopRecording()
		_, err := c.GetState()
		assertNoError(err, t, "GetState()")

		// try rerecording
		go func() {
			c.RestartFrom(true, "", false, nil, [3]string{}, false)
		}()

		time.Sleep(time.Second) // hopefully the re-recording started...
		c.StopRecording()
		_, err = c.GetState()
		assertNoError(err, t, "GetState()")
	})
}

func TestClearLogicalBreakpoint(t *testing.T) {
	// Clearing a logical breakpoint should clear all associated physical
	// breakpoints.
	// Issue #1955.
	withTestClient2Extended("testinline", t, protest.EnableInlining, [3]string{}, nil, func(c service.Client, fixture protest.Fixture) {
		bp, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.inlineThis"})
		assertNoError(err, t, "CreateBreakpoint()")
		t.Logf("breakpoint set at %#v", bp.Addrs)
		if len(bp.Addrs) < 2 {
			t.Fatal("Wrong number of addresses for main.inlineThis breakpoint")
		}
		_, err = c.ClearBreakpoint(bp.ID)
		assertNoError(err, t, "ClearBreakpoint()")
		bps, err := c.ListBreakpoints(false)
		assertNoError(err, t, "ListBreakpoints()")
		for _, curbp := range bps {
			if curbp.ID == bp.ID {
				t.Errorf("logical breakpoint still exists: %#v", curbp)
				break
			}
		}
	})
}

func TestRedirects(t *testing.T) {
	const (
		infile  = "redirect-input.txt"
		outfile = "redirect-output.txt"
	)
	protest.AllowRecording(t)
	withTestClient2Extended("redirect", t, 0, [3]string{infile, outfile, ""}, nil, func(c service.Client, fixture protest.Fixture) {
		outpath := filepath.Join(fixture.BuildDir, outfile)
		<-c.Continue()
		buf, err := os.ReadFile(outpath)
		assertNoError(err, t, "Reading output file")
		t.Logf("output %q", buf)
		if !strings.HasPrefix(string(buf), "Redirect test") {
			t.Fatalf("Wrong output %q", string(buf))
		}
		os.Remove(outpath)
		if testBackend != "rr" {
			_, err = c.Restart(false)
			assertNoError(err, t, "Restart")
			<-c.Continue()
			buf2, err := os.ReadFile(outpath)
			t.Logf("output %q", buf2)
			assertNoError(err, t, "Reading output file (second time)")
			if !strings.HasPrefix(string(buf2), "Redirect test") {
				t.Fatalf("Wrong output %q", string(buf2))
			}
			if string(buf2) == string(buf) {
				t.Fatalf("Expected output change got %q and %q", string(buf), string(buf2))
			}
			os.Remove(outpath)
		}
	})
}

func TestIssue2162(t *testing.T) {
	if buildMode == "pie" || runtime.GOOS == "windows" {
		t.Skip("skip it for stepping into one place where no source for pc when on pie mode or windows")
	}
	withTestClient2("issue2162", t, func(c service.Client) {
		state, err := c.GetState()
		assertNoError(err, t, "GetState()")
		if state.CurrentThread.Function == nil {
			// Can't call Step if we don't have the source code of the current function
			return
		}

		_, err = c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.main"})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		_, err = c.Step()
		assertNoError(err, t, "Step()")
	})
}

func TestDetachLeaveRunning(t *testing.T) {
	// See https://github.com/go-delve/delve/issues/2259
	if testBackend == "rr" {
		return
	}

	listener, clientConn := service.ListenerPipe()
	defer listener.Close()
	var buildFlags protest.BuildFlags
	if buildMode == "pie" {
		buildFlags |= protest.BuildModePIE
	}
	fixture := protest.BuildFixture(t, "testnextnethttp", buildFlags)

	cmd := exec.Command(fixture.Path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	assertNoError(cmd.Start(), t, "starting fixture")
	defer cmd.Process.Kill()

	// wait for testnextnethttp to start listening
	t0 := time.Now()
	for {
		conn, err := net.Dial("tcp", "127.0.0.1:9191")
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
		if time.Since(t0) > 10*time.Second {
			t.Fatal("fixture did not start")
		}
	}

	server := rpccommon.NewServer(&service.Config{
		Listener:   listener,
		APIVersion: 2,
		Debugger: debugger.Config{
			AttachPid:  cmd.Process.Pid,
			WorkingDir: ".",
			Backend:    testBackend,
		},
	})
	if err := server.Run(); err != nil {
		t.Fatal(err)
	}

	client := rpc2.NewClientFromConn(clientConn)
	defer server.Stop()
	assertNoError(client.Detach(false), t, "Detach")
}

func assertNoDuplicateBreakpoints(t *testing.T, c service.Client) {
	t.Helper()
	bps, _ := c.ListBreakpoints(false)
	seen := make(map[int]bool)
	for _, bp := range bps {
		t.Logf("%#v\n", bp)
		if seen[bp.ID] {
			t.Fatalf("duplicate breakpoint ID %d", bp.ID)
		}
		seen[bp.ID] = true
	}
}

func TestToggleBreakpointRestart(t *testing.T) {
	// Checks that breakpoints IDs do not overlap after Restart if there are disabled breakpoints.
	withTestClient2("testtoggle", t, func(c service.Client) {
		bp1, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.main", Line: 1, Name: "firstbreakpoint"})
		assertNoError(err, t, "CreateBreakpoint 1")
		_, err = c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.main", Line: 2, Name: "secondbreakpoint"})
		assertNoError(err, t, "CreateBreakpoint 2")
		_, err = c.ToggleBreakpoint(bp1.ID)
		assertNoError(err, t, "ToggleBreakpoint")
		_, err = c.Restart(false)
		assertNoError(err, t, "Restart")
		assertNoDuplicateBreakpoints(t, c)
		_, err = c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.main", Line: 3, Name: "thirdbreakpoint"})
		assertNoError(err, t, "CreateBreakpoint 3")
		assertNoDuplicateBreakpoints(t, c)
	})
}

func TestStopServerWithClosedListener(t *testing.T) {
	// Checks that the error returned by listener.Accept() is ignored when we
	// are trying to shutdown. See issue #1633.
	if testBackend == "rr" || buildMode == "pie" {
		t.Skip("N/A")
	}
	listener, err := net.Listen("tcp", "localhost:0")
	assertNoError(err, t, "listener")
	fixture := protest.BuildFixture(t, "math", 0)
	server := rpccommon.NewServer(&service.Config{
		Listener:           listener,
		AcceptMulti:        false,
		APIVersion:         2,
		CheckLocalConnUser: true,
		DisconnectChan:     make(chan struct{}),
		ProcessArgs:        []string{fixture.Path},
		Debugger: debugger.Config{
			WorkingDir:  ".",
			Backend:     "default",
			Foreground:  false,
			BuildFlags:  "",
			ExecuteKind: debugger.ExecutingGeneratedFile,
		},
	})
	assertNoError(server.Run(), t, "blah")
	time.Sleep(1 * time.Second) // let server start
	server.Stop()
	listener.Close()
	time.Sleep(1 * time.Second) // give time to server to panic
}

func TestGoroutinesGrouping(t *testing.T) {
	// Tests the goroutine grouping and filtering feature
	withTestClient2("goroutinegroup", t, func(c service.Client) {
		state := <-c.Continue()
		assertNoError(state.Err, t, "Continue")
		_, ggrp, _, _, err := c.ListGoroutinesWithFilter(0, 0, nil, &api.GoroutineGroupingOptions{GroupBy: api.GoroutineLabel, GroupByKey: "name", MaxGroupMembers: 5, MaxGroups: 10}, nil)
		assertNoError(err, t, "ListGoroutinesWithFilter (group by label)")
		t.Logf("%#v\n", ggrp)
		if len(ggrp) < 5 {
			t.Errorf("not enough groups %d\n", len(ggrp))
		}
		var unnamedCount int
		for i := range ggrp {
			if ggrp[i].Name == "name=" {
				unnamedCount = ggrp[i].Total
				break
			}
		}
		gs, _, _, _, err := c.ListGoroutinesWithFilter(0, 0, []api.ListGoroutinesFilter{{Kind: api.GoroutineLabel, Arg: "name="}}, nil, nil)
		assertNoError(err, t, "ListGoroutinesWithFilter (filter unnamed)")
		if len(gs) != unnamedCount {
			t.Errorf("wrong number of goroutines returned by filter: %d (expected %d)\n", len(gs), unnamedCount)
		}
	})
}

func TestLongStringArg(t *testing.T) {
	// Test the ability to load more elements of a string argument, this could
	// be broken if registerized variables are not handled correctly.
	withTestClient2("morestringarg", t, func(c service.Client) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.f"})
		assertNoError(err, t, "CreateBreakpoint")
		state := <-c.Continue()
		assertNoError(state.Err, t, "Continue")

		test := func(name, val1, val2 string) uint64 {
			var1, err := c.EvalVariable(api.EvalScope{GoroutineID: -1}, name, normalLoadConfig)
			assertNoError(err, t, "EvalVariable")
			t.Logf("%#v\n", var1)
			if var1.Value != val1 {
				t.Fatalf("wrong value for variable: %q", var1.Value)
			}
			var2, err := c.EvalVariable(api.EvalScope{GoroutineID: -1}, fmt.Sprintf("(*(*%q)(%#x))[64:]", var1.Type, var1.Addr), normalLoadConfig)
			assertNoError(err, t, "EvalVariable")
			t.Logf("%#v\n", var2)
			if var2.Value != val2 {
				t.Fatalf("wrong value for variable: %q", var2.Value)
			}
			return var1.Addr
		}

		saddr := test("s", "very long string 01234567890123456789012345678901234567890123456", "7890123456789012345678901234567890123456789X")
		test("q", "very long string B 012345678901234567890123456789012345678901234", "567890123456789012345678901234567890123456789X2")
		saddr2 := test("s", "very long string 01234567890123456789012345678901234567890123456", "7890123456789012345678901234567890123456789X")
		if saddr != saddr2 {
			t.Fatalf("address of s changed (%#x %#x)", saddr, saddr2)
		}
	})
}

func TestGenericsBreakpoint(t *testing.T) {
	if !goversion.VersionAfterOrEqual(runtime.Version(), 1, 18) {
		t.Skip("generics")
	}
	// Tests that setting breakpoints inside a generic function with multiple
	// instantiations results in a single logical breakpoint with N physical
	// breakpoints (N = number of instantiations).
	withTestClient2("genericbp", t, func(c service.Client) {
		fp := testProgPath(t, "genericbp")
		bp, err := c.CreateBreakpoint(&api.Breakpoint{File: fp, Line: 6})
		assertNoError(err, t, "CreateBreakpoint")
		if len(bp.Addrs) != 2 {
			t.Fatalf("wrong number of physical breakpoints: %d", len(bp.Addrs))
		}

		frame1Line := func() int {
			frames, err := c.Stacktrace(-1, 10, 0, nil)
			assertNoError(err, t, "Stacktrace")
			return frames[1].Line
		}

		state := <-c.Continue()
		assertNoError(state.Err, t, "Continue")
		if line := frame1Line(); line != 10 {
			t.Errorf("wrong line after first continue, expected 10, got %d", line)
		}

		state = <-c.Continue()
		assertNoError(state.Err, t, "Continue")
		if line := frame1Line(); line != 11 {
			t.Errorf("wrong line after first continue, expected 11, got %d", line)
		}

		if bp.FunctionName != "main.testfn" {
			t.Errorf("wrong name for breakpoint (CreateBreakpoint): %q", bp.FunctionName)
		}

		bps, err := c.ListBreakpoints(false)
		assertNoError(err, t, "ListBreakpoints")

		for _, bp := range bps {
			if bp.ID > 0 {
				if bp.FunctionName != "main.testfn" {
					t.Errorf("wrong name for breakpoint (ListBreakpoints): %q", bp.FunctionName)
				}
				break
			}
		}

		rmbp, err := c.ClearBreakpoint(bp.ID)
		assertNoError(err, t, "ClearBreakpoint")
		if rmbp.FunctionName != "main.testfn" {
			t.Errorf("wrong name for breakpoint (ClearBreakpoint): %q", rmbp.FunctionName)
		}
	})
}

func TestRestartRewindAfterEnd(t *testing.T) {
	if testBackend != "rr" {
		t.Skip("not relevant")
	}
	// Check that Restart works after the program has terminated, even if a
	// Continue is requested just before it.
	// Also check that Rewind can be used after the program has terminated.
	protest.AllowRecording(t)
	withTestClient2("math", t, func(c service.Client) {
		state := <-c.Continue()
		if !state.Exited {
			t.Fatalf("program did not exit")
		}
		state = <-c.Continue()
		if !state.Exited {
			t.Errorf("bad Continue return state: %v", state)
		}
		time.Sleep(1 * time.Second) // bug only happens if there is some time for the server to close the notify channel
		_, err := c.Restart(false)
		if err != nil {
			t.Fatalf("Restart: %v", err)
		}
		state = <-c.Continue()
		if !state.Exited {
			t.Fatalf("program did not exit exited")
		}
		_, err = c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.main", Line: 0})
		if err != nil {
			t.Fatalf("CreateBreakpoint: %v", err)
		}
		state = <-c.Rewind()
		if state.Exited || state.Err != nil {
			t.Errorf("bad Rewind return state: %v", state)
		}
		if state.CurrentThread.Line != 7 {
			t.Errorf("wrong stop location %s:%d", state.CurrentThread.File, state.CurrentThread.Line)
		}
	})
}

func TestClientServer_SinglelineStringFormattedWithBigInts(t *testing.T) {
	// Check that variables that represent large numbers are represented correctly when using a formatting string

	if runtime.GOARCH != "amd64" {
		t.Skip("N/A")
	}
	withTestClient2Extended("xmm0print/", t, 0, [3]string{}, nil, func(c service.Client, fixture protest.Fixture) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.VPSLLQ36", Line: 4})
		assertNoError(err, t, "CreateBreakpoint")
		state := <-c.Continue()
		if state.CurrentThread.Line != 8 {
			t.Fatalf("wrong location after continue %s:%d", state.CurrentThread.File, state.CurrentThread.Line)
		}

		constvar, err := c.EvalVariable(api.EvalScope{GoroutineID: -1}, "9331634762088972288", normalLoadConfig)
		assertNoError(err, t, "ErrVariable(9331634762088972288)")
		out := constvar.SinglelineStringFormatted("%X")
		t.Logf("constant: %q\n", out)
		if out != "8180A06000000000" {
			t.Errorf("expected \"8180A06000000000\" got %q when printing constant", out)
		}

		xmm0var, err := c.EvalVariable(api.EvalScope{GoroutineID: -1}, "XMM0.uint64", normalLoadConfig)
		assertNoError(err, t, "EvalVariable(XMM0.uint64)")

		expected := []string{
			"9331634762088972288", "8180A06000000000",
			"9331634762088972288", "8180A06000000000",
			"9259436018245828608", "8080200000000000",
			"9259436018245828608", "8080200000000000",
			"0", "0", "0", "0",
			"0", "0", "0", "0",
		}

		for i := range xmm0var.Children {
			child := &xmm0var.Children[i]
			if child.Kind != reflect.Uint64 {
				t.Errorf("wrong kind for variable %s\n", child.Kind)
			}
			out1 := child.SinglelineString()
			out2 := child.SinglelineStringFormatted("%X")
			t.Logf("%q %q\n", out1, out2)
			if out1 != expected[i*2] {
				t.Errorf("for child %d expected %s got %s (decimal)", i, expected[i*2], out1)
			}
			if out2 != expected[i*2+1] {
				t.Errorf("for child %d expected %s got %s (hexadecimal)", i, expected[i*2+1], out2)
			}
		}
	})
}

func TestNonGoDebug(t *testing.T) {
	// Test that we can at least set breakpoints while debugging a non-go executable.
	if runtime.GOOS != "linux" {
		t.Skip()
	}
	if objcopyPath, _ := exec.LookPath("cc"); objcopyPath == "" {
		t.Skip("no C compiler in path")
	}
	dir := protest.FindFixturesDir()
	path := protest.TempFile("testc")
	cmd := exec.Command("cc", "-g", "-o", path, filepath.Join(dir, "test.c"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Error compiling %s: %s\n%s", path, err, out)
	}

	listener, clientConn := service.ListenerPipe()
	defer listener.Close()

	server := rpccommon.NewServer(&service.Config{
		Listener:    listener,
		ProcessArgs: []string{path},
		Debugger: debugger.Config{
			Backend:     testBackend,
			ExecuteKind: debugger.ExecutingExistingFile,
		},
	})

	if err := server.Run(); err != nil {
		t.Fatal(err)
	}

	client := rpc2.NewClientFromConn(clientConn)
	defer func() {
		client.Detach(true)
	}()

	_, err := client.CreateBreakpoint(&api.Breakpoint{FunctionName: "C.main", Line: -1})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRestart_PreserveFunctionBreakpoint(t *testing.T) {
	// Tests that function breakpoint get restored correctly, after a rebuild,
	// even if the function changed position in the source file.

	dir := protest.FindFixturesDir()
	outpath := filepath.Join(dir, "testfnpos.go")
	defer os.Remove(outpath)

	copy := func(inpath string) {
		buf, err := os.ReadFile(inpath)
		assertNoError(err, t, fmt.Sprintf("Reading %q", inpath))
		assertNoError(os.WriteFile(outpath, buf, 0o666), t, fmt.Sprintf("Creating %q", outpath))
	}

	copy(filepath.Join(dir, "testfnpos1.go"))

	withTestClient2Extended("testfnpos", t, 0, [3]string{}, nil, func(c service.Client, f protest.Fixture) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.f1"})
		assertNoError(err, t, "CreateBreakpoint")
		state := <-c.Continue()
		assertNoError(state.Err, t, "Continue")
		t.Logf("%s:%d", state.CurrentThread.File, state.CurrentThread.Line)
		if state.CurrentThread.Line != 5 {
			t.Fatalf("wrong location %s:%d", state.CurrentThread.File, state.CurrentThread.Line)
		}

		// rewrite test file and restart, rebuilding
		copy(filepath.Join(dir, "testfnpos2.go"))
		_, err = c.Restart(true)
		assertNoError(err, t, "Restart(true)")

		state = <-c.Continue()
		assertNoError(state.Err, t, "Continue")
		t.Logf("%s:%d", state.CurrentThread.File, state.CurrentThread.Line)
		if state.CurrentThread.Line != 9 {
			t.Fatalf("wrong location %s:%d", state.CurrentThread.File, state.CurrentThread.Line)
		}
	})
}

func assertLine(t *testing.T, state *api.DebuggerState, file string, lineno int) {
	t.Helper()
	t.Logf("%s:%d", state.CurrentThread.File, state.CurrentThread.Line)
	if !strings.HasSuffix(state.CurrentThread.File, file) || state.CurrentThread.Line != lineno {
		t.Fatalf("wrong location %s:%d", state.CurrentThread.File, state.CurrentThread.Line)
	}
}

func TestPluginSuspendedBreakpoint(t *testing.T) {
	if runtime.GOARCH == "ppc64le" {
		t.Skip("skipped on ppc64le: broken")
	}
	// Tests that breakpoints created in a suspended state will be enabled automatically when a plugin is loaded.
	pluginFixtures := protest.WithPlugins(t, protest.AllNonOptimized, "plugin1/", "plugin2/")
	dir, err := filepath.Abs(protest.FindFixturesDir())
	assertNoError(err, t, "filepath.Abs")

	withTestClient2Extended("plugintest", t, protest.AllNonOptimized, [3]string{}, []string{pluginFixtures[0].Path, pluginFixtures[1].Path}, func(c service.Client, f protest.Fixture) {
		_, err := c.CreateBreakpointWithExpr(&api.Breakpoint{FunctionName: "github.com/go-delve/delve/_fixtures/plugin1.Fn1", Line: 1}, "", nil, true)
		assertNoError(err, t, "CreateBreakpointWithExpr(Fn1) (suspended)")

		_, err = c.CreateBreakpointWithExpr(&api.Breakpoint{File: filepath.Join(dir, "plugin2", "plugin2.go"), Line: 9}, "", nil, true)
		assertNoError(err, t, "CreateBreakpointWithExpr(plugin2.go:9) (suspended)")

		cont := func(name, file string, lineno int) {
			t.Helper()
			state := <-c.Continue()
			assertNoError(state.Err, t, name)
			assertLine(t, state, file, lineno)
		}

		cont("Continue 1", "plugintest.go", 22)
		cont("Continue 2", "plugintest.go", 27)
		cont("Continue 3", "plugin1.go", 6)
		cont("Continue 4", "plugin2.go", 9)
	})

	withTestClient2Extended("plugintest", t, protest.AllNonOptimized, [3]string{}, []string{pluginFixtures[0].Path, pluginFixtures[1].Path}, func(c service.Client, f protest.Fixture) {
		exprbreak := func(expr string) {
			t.Helper()
			_, err := c.CreateBreakpointWithExpr(&api.Breakpoint{}, expr, nil, true)
			assertNoError(err, t, fmt.Sprintf("CreateBreakpointWithExpr(%s) (suspended)", expr))
		}

		cont := func(name, file string, lineno int) {
			t.Helper()
			state := <-c.Continue()
			assertNoError(state.Err, t, name)
			assertLine(t, state, file, lineno)
		}

		exprbreak("plugin1.Fn1")
		exprbreak("plugin2.go:9")

		// The following breakpoints can never be un-suspended because the
		// expression is never resolved, but this shouldn't cause problems

		exprbreak("m[0]")
		exprbreak("*m[0]")
		exprbreak("unknownfn")
		exprbreak("+2")

		cont("Continue 1", "plugintest.go", 22)
		cont("Continue 2", "plugintest.go", 27)
		cont("Continue 3", "plugin1.go", 5)
		cont("Continue 4", "plugin2.go", 9)
	})
}

// Tests that breakpoint set after the process has exited will be hit when the process is restarted.
func TestBreakpointAfterProcessExit(t *testing.T) {
	withTestClient2("continuetestprog", t, func(c service.Client) {
		state := <-c.Continue()
		if !state.Exited {
			t.Fatal("process should have exited")
		}
		bp, err := c.CreateBreakpointWithExpr(&api.Breakpoint{ID: 2, FunctionName: "main.main", Line: 1}, "main.main", nil, true)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Restart(false)
		if err != nil {
			t.Fatal(err)
		}
		state = <-c.Continue()
		if state.CurrentThread == nil {
			t.Fatal("no current thread")
		}
		if state.CurrentThread.Breakpoint == nil {
			t.Fatal("no breakpoint")
		}
		if state.CurrentThread.Breakpoint.ID != bp.ID {
			t.Fatal("did not hit correct breakpoint")
		}
		if state.CurrentThread.Function == nil {
			t.Fatal("no function")
		}
		if state.CurrentThread.Function.Name() != "main.main" {
			t.Fatal("stopped at incorrect function")
		}
		state = <-c.Continue()
		if !state.Exited {
			t.Fatal("process should have exited")
		}
		_, err = c.ClearBreakpoint(bp.ID)
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestClientServer_createBreakpointWithID(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("continuetestprog", t, func(c service.Client) {
		bp, err := c.CreateBreakpoint(&api.Breakpoint{ID: 2, FunctionName: "main.main", Line: 1})
		assertNoError(err, t, "CreateBreakpoint()")
		if bp.ID != 2 {
			t.Errorf("wrong ID for breakpoint %d", bp.ID)
		}

		bp2, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "main.main", Line: 2})
		assertNoError(err, t, "CreateBreakpoint()")
		if bp2.ID != 3 {
			t.Errorf("wrong ID for breakpoint %d", bp2.ID)
		}
	})
}

func TestClientServer_autoBreakpoints(t *testing.T) {
	// Check that unrecovered-panic and fatal-throw breakpoints are visible in
	// the breakpoint list.
	protest.AllowRecording(t)
	withTestClient2("math", t, func(c service.Client) {
		bps, err := c.ListBreakpoints(false)
		assertNoError(err, t, "ListBreakpoints")
		n := 0
		for _, bp := range bps {
			t.Log(bp)
			if bp.Name == proc.UnrecoveredPanic || bp.Name == proc.FatalThrow {
				n++
			}
		}
		if n != 2 {
			t.Error("automatic breakpoints not found")
		}
	})
}

func TestClientServer_breakpointOnFuncWithABIWrapper(t *testing.T) {
	// Setting a breakpoint on an assembly function that has an ABI
	// compatibility wrapper should end up setting a breakpoint on the real
	// function (also setting a breakpoint on the wrapper is fine).
	// Issue #3296
	protest.AllowRecording(t)
	withTestClient2("math", t, func(c service.Client) {
		bp, err := c.CreateBreakpoint(&api.Breakpoint{FunctionName: "runtime.schedinit"})
		assertNoError(err, t, "CreateBreakpoint()")
		t.Log(bp)

		found := false
		for _, pc := range bp.Addrs {
			text, err := c.DisassemblePC(api.EvalScope{}, pc, api.IntelFlavour)
			assertNoError(err, t, fmt.Sprint("DisassemblePC", pc))
			t.Log("First instruction for", pc, text[0])
			if strings.HasSuffix(text[0].Loc.File, "runtime/proc.go") {
				found = true
			}
		}
		if !found {
			t.Error("breakpoint not set on the runtime/proc.go function")
		}
	})
}

var waitReasonStrings = [...]string{
	"",
	"GC assist marking",
	"IO wait",
	"chan receive (nil chan)",
	"chan send (nil chan)",
	"dumping heap",
	"garbage collection",
	"garbage collection scan",
	"panicwait",
	"select",
	"select (no cases)",
	"GC assist wait",
	"GC sweep wait",
	"GC scavenge wait",
	"chan receive",
	"chan send",
	"finalizer wait",
	"force gc (idle)",
	"semacquire",
	"sleep",
	"sync.Cond.Wait",
	"timer goroutine (idle)",
	"trace reader (blocked)",
	"wait for GC cycle",
	"GC worker (idle)",
	"preempted",
	"debug call",
}

func TestClientServer_chanGoroutines(t *testing.T) {
	withTestClient2("changoroutines", t, func(c service.Client) {
		state := <-c.Continue()
		assertNoError(state.Err, t, "Continue()")

		countRecvSend := func(gs []*api.Goroutine) (recvq, sendq int) {
			for _, g := range gs {
				t.Logf("\tID: %d WaitReason: %s\n", g.ID, waitReasonStrings[g.WaitReason])
				switch waitReasonStrings[g.WaitReason] {
				case "chan send":
					sendq++
				case "chan receive":
					recvq++
				}
			}
			return
		}

		gs, _, _, _, err := c.ListGoroutinesWithFilter(0, 100, []api.ListGoroutinesFilter{{Kind: api.GoroutineWaitingOnChannel, Arg: "blockingchan1"}}, nil, &api.EvalScope{GoroutineID: -1})
		assertNoError(err, t, "ListGoroutinesWithFilter(blockingchan1)")
		t.Logf("blockingchan1 gs:")
		recvq, sendq := countRecvSend(gs)
		if len(gs) != 2 || recvq != 0 || sendq != 2 {
			t.Error("wrong number of goroutines for blockingchan1")
		}

		gs, _, _, _, err = c.ListGoroutinesWithFilter(0, 100, []api.ListGoroutinesFilter{{Kind: api.GoroutineWaitingOnChannel, Arg: "blockingchan2"}}, nil, &api.EvalScope{GoroutineID: -1})
		assertNoError(err, t, "ListGoroutinesWithFilter(blockingchan2)")
		t.Logf("blockingchan2 gs:")
		recvq, sendq = countRecvSend(gs)
		if len(gs) != 1 || recvq != 1 || sendq != 0 {
			t.Error("wrong number of goroutines for blockingchan2")
		}
	})
}

func TestNextInstruction(t *testing.T) {
	protest.AllowRecording(t)
	withTestClient2("testprog", t, func(c service.Client) {
		fp := testProgPath(t, "testprog")
		_, err := c.CreateBreakpoint(&api.Breakpoint{File: fp, Line: 19})
		assertNoError(err, t, "CreateBreakpoint()")
		state := <-c.Continue()
		assertNoError(state.Err, t, "Continue()")

		state, err = c.StepInstruction(true)
		assertNoError(err, t, "Step()")
		if state.CurrentThread.Line != 20 {
			t.Fatalf("expected line %d got %d", 20, state.CurrentThread.Line)
		}
	})
}

func TestBreakpointVariablesWithoutG(t *testing.T) {
	// Tests that evaluating variables on a breakpoint that is hit on a thread
	// without a goroutine does not cause an error.
	withTestClient2("math", t, func(c service.Client) {
		_, err := c.CreateBreakpoint(&api.Breakpoint{
			FunctionName: "runtime.mallocgc",
			LoadArgs:     &normalLoadConfig,
		})
		assertNoError(err, t, "CreateBreakpoint")
		state := <-c.Continue()
		assertNoError(state.Err, t, "Continue()")
	})
}

func TestGuessSubstitutePath(t *testing.T) {
	protest.MustHaveModules(t)

	t.Setenv("NOCERT", "1")
	ver, _ := goversion.Parse(runtime.Version())
	if ver.IsDevelBuild() && os.Getenv("CI") != "" && runtime.GOOS == "linux" {
		// The TeamCity builders for linux/amd64/tip and linux/arm64/tip end up
		// with a broken .git directory which makes the 'go list' command used for
		// GuessSubstitutePath fail.
		t.Skip("does not work in TeamCity + tip + linux")
	}

	slashnorm := func(s string) string {
		if runtime.GOOS != "windows" {
			return s
		}
		return strings.ReplaceAll(s, "\\", "/")
	}

	guess := func(t *testing.T, goflags string) [][2]string {
		oldgoflags := os.Getenv("GOFLAGS")
		os.Setenv("GOFLAGS", goflags)
		defer os.Setenv("GOFLAGS", oldgoflags)

		dlvbin := protest.GetDlvBinary(t)

		listener, clientConn := service.ListenerPipe()
		defer listener.Close()
		server := rpccommon.NewServer(&service.Config{
			Listener:    listener,
			ProcessArgs: []string{dlvbin, "help"},
			Debugger: debugger.Config{
				Backend:        testBackend,
				CheckGoVersion: true,
				BuildFlags:     "", // build flags can be an empty string here because the only test that uses it, does not set special flags.
				ExecuteKind:    debugger.ExecutingExistingFile,
			},
		})
		if err := server.Run(); err != nil {
			t.Fatal(err)
		}

		client := rpc2.NewClientFromConn(clientConn)
		defer client.Detach(true)

		switch runtime.GOARCH {
		case "ppc64le":
			os.Setenv("GOFLAGS", "-tags=exp.linuxppc64le")
		case "riscv64":
			os.Setenv("GOFLAGS", "-tags=exp.linuxriscv64")
		case "loong64":
			os.Setenv("GOFLAGS", "-tags=exp.linuxloong64")
		}

		gsp, err := client.GuessSubstitutePath()
		assertNoError(err, t, "GuessSubstitutePath")
		return gsp
	}

	delvePath := protest.ProjectRoot()
	var nmods int = -1

	t.Run("Normal", func(t *testing.T) {
		gsp := guess(t, "")
		t.Logf("Normal build: %d", len(gsp))
		if len(gsp) == 0 {
			t.Fatalf("not enough modules")
		}
		found := false
		for _, e := range gsp {
			t.Logf("\t%s -> %s", e[0], e[1])
			if e[0] != slashnorm(e[1]) {
				t.Fatalf("mismatch %q %q", e[0], e[1])
			}
			if e[1] == delvePath {
				found = true
			}
		}
		nmods = len(gsp)
		if !found {
			t.Fatalf("could not find main module path %q", delvePath)
		}

		if os.Getenv("CI") == "true" {
			return
		}
	})

	t.Run("Modules", func(t *testing.T) {
		gsp := guess(t, "-mod=mod")
		t.Logf("Modules build: %d", len(gsp))
		if len(gsp) != nmods && nmods != -1 {
			t.Fatalf("not enough modules")
		}
		found := false
		for _, e := range gsp {
			t.Logf("\t%s -> %s", e[0], e[1])
			if e[0] == slashnorm(delvePath) && e[1] == delvePath {
				found = true
			}
		}
		if !found {
			t.Fatalf("could not find main module path %q", delvePath)
		}
	})

	t.Run("Trimpath", func(t *testing.T) {
		gsp := guess(t, "-trimpath")
		t.Logf("Trimpath build: %d", len(gsp))
		if len(gsp) != nmods && nmods != -1 {
			t.Fatalf("not enough modules")
		}
		found := false
		for _, e := range gsp {
			t.Logf("\t%s -> %s", e[0], e[1])
			if e[0] == "github.com/go-delve/delve" && e[1] == delvePath {
				found = true
			}
		}
		if !found {
			t.Fatalf("could not find main module path %q", delvePath)
		}
	})

	t.Run("ModulesTrimpath", func(t *testing.T) {
		gsp := guess(t, "-trimpath -mod=mod")
		t.Logf("Modules+Trimpath build: %d", len(gsp))
		if len(gsp) != nmods && nmods != -1 {
			t.Fatalf("not enough modules")
		}
		found := false
		for _, e := range gsp {
			t.Logf("\t%s -> %s", e[0], e[1])
			if e[0] == "github.com/go-delve/delve" && e[1] == delvePath {
				found = true
			}
		}
		if !found {
			t.Fatalf("could not find main module path %q", delvePath)
		}
	})
}

func TestFollowExecFindLocation(t *testing.T) {
	// FindLocation should not return an error if at least one of the currently
	// attached targets can find the specified location.
	// See issue #3933
	if runtime.GOOS == "freebsd" || runtime.GOOS == "darwin" {
		t.Skip("follow exec not implemented")
	}
	var buildFlags protest.BuildFlags
	if buildMode == "pie" {
		buildFlags |= protest.BuildModePIE
	}
	childFixture := protest.BuildFixture(t, "spawnchild", buildFlags)

	withTestClient2Extended("spawn", t, 0, [3]string{}, []string{"spawn2", childFixture.Path}, func(c service.Client, fixture protest.Fixture) {
		assertNoError(c.FollowExec(true, ""), t, "FollowExec")
		_, err := c.CreateBreakpointWithExpr(&api.Breakpoint{File: childFixture.Source, Line: 9}, fmt.Sprintf("%s:%d", childFixture.Source, 9), nil, true)
		assertNoError(err, t, "CreateBreakpoint(spawnchild.go:9)")

		state := <-c.Continue()
		assertNoError(state.Err, t, "Continue()")

		tgts, err := c.ListTargets()
		assertNoError(err, t, "ListTargets")

		t.Logf("%v\n", tgts)
		found := false
		for _, tgt := range tgts {
			if tgt.Pid == state.Pid {
				if !strings.Contains(tgt.CmdLine, "spawnchild") {
					t.Fatalf("did not switch to child process")
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("current target not found")
		}

		_, _, err = c.FindLocation(api.EvalScope{GoroutineID: -1}, fmt.Sprintf("%s:%d", childFixture.Source, 6), true, nil)
		assertNoError(err, t, "FindLocation(spawnchild.go:6)")

		_, _, err = c.FindLocation(api.EvalScope{GoroutineID: -1}, fmt.Sprintf("%s:%d", fixture.Source, 19), true, nil)
		assertNoError(err, t, "FindLocation(spawn.go:19)")
	})
}
