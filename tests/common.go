// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Common test setup code
package tests

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/cloudsql-proxy/logging"
	"github.com/GoogleCloudPlatform/cloudsql-proxy/proxy/proxy"

	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"
)

var (
	project      = flag.String("project", "", "Project to create the GCE test VM in")
	zone         = flag.String("zone", "us-central1-f", "Zone in which to create the VM")
	osImage      = flag.String("os", defaultOS, "OS image to use when creating a VM")
	vmName       = flag.String("vm_name", "proxy-test-gce", "Name of VM to create")
	databaseName = flag.String("db_name", "", "Fully-qualified Cloud SQL Instance (in the form of 'project:region:instance-name')")
)

const (
	defaultOS       = "https://www.googleapis.com/compute/v1/projects/debian-cloud/global/images/debian-8-jessie-v20160329"
	testTimeout     = 3 * time.Minute
	buildShLocation = "cmd/cloud_sql_proxy/build.sh"
)

func setupGCEProxy(t *testing.T, proxyArgs []string) (error, *ssh.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	proxyBinary, err := compileProxy()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Built new cloud_sql_proxy binary")
	defer os.Remove(proxyBinary)

	cl, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		t.Fatal(err)
	}

	ssh, err := newOrReuseVM(t.Logf, cl)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SSH to %s:%s succeeded", *project, *vmName)

	log.Printf("apt-get update...")
	var sout, serr bytes.Buffer
	if err := sshRun(ssh, "sudo apt-get update", nil, &sout, &serr); err != nil {
		t.Fatalf("Failed 'sudo apt-get update' on remote machine: %v\n\nstandard out:\n%s\nstandard err:\n%s", err, &sout, &serr)
	}
	log.Printf("Install mysql client...")
	if err := sshRun(ssh, "sudo apt-get install -y mysql-client", nil, &sout, &serr); err != nil {
		t.Fatalf("Failed 'sudo apt-get install -y mysql-client' on remote machine: %v\n\nstandard out:\n%s\nstandard err:\n%s", err, &sout, &serr)
	}
	if err = sshRun(ssh, "pkill cloud_sql_proxy", nil, &sout, &serr); err != nil {
		t.Logf("Failed to kill any cloud_sql_proxy process.")
	} else {
		t.Logf("Killed already running cloud_sql_proxy process.")
	}
	log.Printf("Copy binary to %s:%s...", *project, *vmName)
	this, err := os.Open(proxyBinary)
	if err != nil {
		t.Fatalf("Couldn't open %v for reading: %v", proxyBinary, err)
	}
	err = sshRun(ssh, "bash -c 'cat >cloud_sql_proxy; chmod +x cloud_sql_proxy; mkdir -p cloudsql'", this, &sout, &serr)
	this.Close()
	if err != nil {
		t.Fatalf("Couldn't scp to remote machine: %v\n\nstandard out:\n%s\nstandard err:\n%s", err, &sout, &serr)
	}
	logs, err := startProxy(ssh, "./cloud_sql_proxy -dir cloudsql -instances "+strings.Join(append([]string{*databaseName}, proxyArgs...), " "))
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	// TODO: Instead of discarding all of the logs, verify that certain logs
	// happen during connects/disconnects.
	go io.Copy(ioutil.Discard, logs)
	t.Logf("Cloud SQL Proxy started on remote host")
	return err, ssh
}

var _ io.ReadCloser = (*process)(nil)

// process wraps a remotely executing process, turning it into an
// io.ReadCloser.
type process struct {
	io.Reader
	sess *ssh.Session
}

// TODO: Return the value of 'Wait'ing on the process. ssh.Session.Signal
// doesn't seem to have an effect, so calling it and then doing Wait doesn't do
// anything. Closing the session is the only way to clean up until I figure out
// what's wrong.
func (p *process) Close() error {
	return p.sess.Close()
}

func compileProxy() (binaryPath string, _ error) {
	// Find the 'build.sh' script by looking for it in cwd, cwd/.., and cwd/../..
	var buildSh string

	var parentPath []string
	for parents := 0; parents < 2; parents++ {
		cur := filepath.Join(append(parentPath, buildShLocation)...)
		if _, err := os.Stat(cur); err == nil {
			buildSh = cur
			break
		}
		parentPath = append(parentPath, "..")
	}
	if buildSh == "" {
		return "", fmt.Errorf("couldn't find %q; please cd into the local repository", buildShLocation)
	}

	cmd := exec.Command(buildSh)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("error during build.sh execution: %v;\n%s", err, out)
	}

	return "cloud_sql_proxy", nil
}

// startProxy executes the cloud_sql_proxy via ssh. The returned ReadCloser
// must be serviced and closed when finished, otherwise the SSH connection may
// block.
func startProxy(ssh *ssh.Client, args string) (io.ReadCloser, error) {
	sess, err := ssh.NewSession()
	if err != nil {
		return nil, fmt.Errorf("couldn't open new session: %v", err)
	}
	pr, err := sess.StderrPipe()
	if err != nil {
		return nil, err
	}
	log.Printf("Running proxy...")
	if err := sess.Start(args); err != nil {
		return nil, err
	}

	// The proxy prints "Ready for new connections" after it starts up
	// correctly. Start a new goroutine looking for that value so that we can
	// time-out appropriately (in case something weird is going on).
	in := bufio.NewReader(pr)
	buf := new(bytes.Buffer)
	errCh := make(chan error, 1)
	go func() {
		for {
			bs, err := in.ReadBytes('\n')
			if err != nil {
				if err == io.EOF {
					log.Print("reading stderr gave EOF (remote process closed)")
					err = sess.Wait()
				}
				errCh <- fmt.Errorf("failed to run `%s`: %v", args, err)
				return
			}
			buf.Write(bs)
			if bytes.Contains(bs, []byte("Ready for new connections")) {
				errCh <- nil
				return
			}
		}
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return nil, err
		}

		// Proxy process startup succeeded.
		return &process{
			io.MultiReader(buf, in),
			sess,
		}, nil
	case <-time.After(3 * time.Second):
		log.Printf("Timeout starting up `%v`", args)
	}

	// Starting the proxy timed out, so we should close the SSH session and
	// return an error after the process exits.
	// TODO: the sess.Signal method doesn't seem to work... that's what we
	// really want to do.
	err = sess.Close()
	select {
	case waitErr := <-errCh:
		if err == nil {
			err = waitErr
		}
	case <-time.After(2 * time.Second):
		log.Printf("Timeout while waiting for process after closing SSH session.")
		if err == nil {
			err = errors.New("timeout waiting for SSH connection to close")
		}
	}
	return nil, fmt.Errorf("timeout waiting for `%v`: error from close: %v; output was:\n\n%s", args, err, buf)
}

func sshRun(ssh *ssh.Client, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	sess, err := ssh.NewSession()
	if err != nil {
		return err
	}

	sess.Stdin = stdin
	if stderr == nil && stdout == nil {
		if out, err := sess.CombinedOutput(cmd); err != nil {
			return fmt.Errorf("`%v`: %v; combined output was:\n%s", cmd, err, out)
		}
		return nil
	}
	sess.Stdout = stdout
	sess.Stderr = stderr

	return sess.Run(cmd)
}

func newOrReuseVM(logf func(string, ...interface{}), cl *http.Client) (*ssh.Client, error) {
	c, err := compute.New(cl)
	if err != nil {
		return nil, err
	}

	user := "test-user"
	pub, auth, err := sshKey()
	if err != nil {
		return nil, err
	}
	sshPubKey := user + ":" + pub

	var op *compute.Operation

	if inst, err := c.Instances.Get(*project, *zone, *vmName).Do(); err != nil {
		logf("Creating new instance (getting instance %v in project %v and zone %v failed: %v)", *vmName, *project, *zone, err)
		instProto := &compute.Instance{
			Name:        *vmName,
			MachineType: "zones/" + *zone + "/machineTypes/g1-small",
			Disks: []*compute.AttachedDisk{{
				AutoDelete: true,
				Boot:       true,
				InitializeParams: &compute.AttachedDiskInitializeParams{
					SourceImage: *osImage,
					DiskSizeGb:  10,
				}},
			},
			NetworkInterfaces: []*compute.NetworkInterface{{
				Network:       "projects/" + *project + "/global/networks/default",
				AccessConfigs: []*compute.AccessConfig{{Name: "External NAT", Type: "ONE_TO_ONE_NAT"}},
			}},
			Metadata: &compute.Metadata{
				Items: []*compute.MetadataItems{{
					Key: "sshKeys", Value: &sshPubKey,
				}},
			},
			Tags: &compute.Tags{Items: []string{"ssh"}},
			ServiceAccounts: []*compute.ServiceAccount{{
				Email:  "default",
				Scopes: []string{proxy.SQLScope},
			}},
		}
		op, err = c.Instances.Insert(*project, *zone, instProto).Do()
		if err != nil {
			return nil, err
		}
	} else {
		logf("attempting to reuse instance %v (in project %v and zone %v)...", *vmName, *project, *zone)
		set := false
		md := inst.Metadata
		for _, v := range md.Items {
			if v.Key == "sshKeys" {
				v.Value = &sshPubKey
				set = true
				break
			}
		}
		if !set {
			md.Items = append(md.Items, &compute.MetadataItems{Key: "sshKeys", Value: &sshPubKey})
		}
		op, err = c.Instances.SetMetadata(*project, *zone, *vmName, md).Do()
		if err != nil {
			return nil, err
		}
	}

	for {
		if op.Error != nil && len(op.Error.Errors) > 0 {
			return nil, fmt.Errorf("errors: %v", op.Error.Errors)
		}

		log.Printf("%v %v (%v)", op.OperationType, op.TargetLink, op.Status)
		if op.Status == "DONE" {
			break
		}
		time.Sleep(5 * time.Second)

		op, err = c.ZoneOperations.Get(*project, *zone, op.Name).Do()
		if err != nil {
			return nil, err
		}
	}

	inst, err := c.Instances.Get(*project, *zone, *vmName).Do()
	if err != nil {
		return nil, fmt.Errorf("error getting instance after it was created: %v", err)
	}
	ip := inst.NetworkInterfaces[0].NetworkIP

	var lastErr error
	for try := 0; try < 10; try++ {
		if lastErr != nil {
			const sleepTime = 10 * time.Second
			logging.Errorf("%v; sleeping for %v then retrying", lastErr, sleepTime)
			time.Sleep(sleepTime)
		}
		ssh, err := ssh.Dial("tcp", ip+":22", &ssh.ClientConfig{
			User:            user,
			Auth:            []ssh.AuthMethod{auth},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		})
		if err == nil {
			return ssh, nil
		}
		lastErr = fmt.Errorf("couldn't ssh to %v (IP=%v): %v", *vmName, ip, err)
	}
	return nil, lastErr
}

func sshKey() (pubKey string, auth ssh.AuthMethod, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", nil, err
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return "", nil, err
	}
	pub, err := ssh.NewPublicKey(&key.PublicKey)
	if err != nil {
		return "", nil, err
	}
	return string(ssh.MarshalAuthorizedKey(pub)), ssh.PublicKeys(signer), nil
}

func TestMain(m *testing.M) {
	flag.Parse()
	switch "" {
	case *project:
		log.Fatal("Must set -project")
	case *databaseName:
		log.Fatal("Must set -db_name")
	}

	os.Exit(m.Run())
}
