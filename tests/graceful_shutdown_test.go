// Copyright 2018 Google Inc. All Rights Reserved.
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

// graceful_shutdown_test is an integration test meant to verify the proxy's
// graceful shutdown behaviour.
// It works by provisions a GCE VM, loads a newly-compiled proxy client onto that VM, and executes the following serie of commands:
// 1. Start a mysql client running a long running statement.
// 2. SIGTERM the proxy.
// 3. Start another mysql command that we expected to fail as the proxy should not
//    accept new connections.
// 4. Kill all running mysql clients with the intention to drop the proxy connection count down to zero.
//    If this fail no mysql command where found and most likley means that the proxy exited prematurely.
// 5. Verify that the proxy are no longer running.
//
// Required flags:
//    -db_name, -project
//
// Example invocation:
//     go test -v -run TestGracefulShutdown -args -project=my-project -db_name=my-project:the-region:sql-name
//     go test -v -run TestShutdownGracePeriod -args -project=my-project -db_name=my-project:the-region:sql-name
package tests

import (
	"bytes"
	"fmt"
	"log"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// TestGracefulShutdown provisions a new GCE VM and verifies that the proxy's
// graceful shutdown behaviour works as intended.
func TestGracefulShutdown(t *testing.T) {
	err, ssh := setupGCEProxy(t, nil)

	log.Print("Starting blocking mysql command")
	err = runCmd(ssh,
		fmt.Sprintf(`/usr/bin/nohup mysql -uroot -S cloudsql/%s -e "SELECT 1; SELECT SLEEP(120);" > /dev/null &`, *databaseName))
	if err != nil {
		t.Fatal(err)
	}

	log.Printf("Sending SIGTERM to cloud_sql_proxy")
	if err = runCmd(ssh, `pkill -SIGTERM cloud_sql_proxy`); err != nil {
		t.Fatal(err)
	}

	log.Print("Test that no new connections is accepted")
	err = runCmd(ssh, fmt.Sprintf(`mysql -uroot -S cloudsql/%s -e "SELECT 1;`, *databaseName))
	if err == nil {
		t.Fatalf("Mysql connection should have been refused: %v", err)
	}

	// Failing here most likley means that cloud_sql_proxy exited prematurley.
	log.Print("Stop blocking mysql command")
	if err = runCmd(ssh, `pkill mysql`); err != nil {
		t.Fatal(err)
	}

	log.Print("Waiting for cloud_sql_proxy to stop...")
	if err := waitForProxyDeath(ssh, 30*time.Second); err != nil {
		t.Fatal(err)
	}
}

// TestShutdownGracePeriod provisions a new GCE VM and verifies that the proxy
// exits after shutdown_grace_period has passed.
func TestShutdownGracePeriod(t *testing.T) {
	err, ssh := setupGCEProxy(t, []string{"-shutdown_grace_period", "5s"})

	log.Print("Starting blocking mysql command")
	err = runCmd(ssh,
		fmt.Sprintf(`/usr/bin/nohup mysql -uroot -S cloudsql/%s -e "SELECT 1; SELECT SLEEP(120);" > /dev/null &`, *databaseName))
	if err != nil {
		t.Fatal(err)
	}

	log.Printf("Sending SIGTERM to cloud_sql_proxy")
	if err = runCmd(ssh, `pkill -SIGTERM cloud_sql_proxy`); err != nil {
		t.Fatal(err)
	}

	log.Print("Waiting for cloud_sql_proxy to stop...")
	if err := waitForProxyDeath(ssh, 30*time.Second); err != nil {
		t.Fatal(err)
	}
}

var postCmdDelay = time.Second

func runCmd(ssh *ssh.Client, cmd string) error {
	var sout, serr bytes.Buffer
	if err := sshRun(ssh, cmd, nil, &sout, &serr); err != nil {
		return fmt.Errorf("Error running command '%s': %v\n\nstandard out:\n%s\nstandard err:\n%s", cmd, err, &sout, &serr)
	}
	<-time.After(postCmdDelay)
	return nil
}

func waitForProxyDeath(ssh *ssh.Client, maxWait time.Duration) error {
	done := make(chan struct{})
	interval := time.NewTicker(time.Second)
	defer interval.Stop()

	go func() {
		for {
			// If pidof returns a non-zero exit code no program was found with the
			// requested name "cloud_sql_proxy".
			if err := runCmd(ssh, `/bin/pidof cloud_sql_proxy`); err != nil {
				close(done)
				return
			}
			log.Print("cloud_sql_proxy still running...")
			<-interval.C
		}
	}()

	select {
	case <-done:
		return nil
	case <-time.After(maxWait):
		return fmt.Errorf("cloud_sql_proxy did not shutdown within %v", maxWait)
	}
}
