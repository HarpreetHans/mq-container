/*
© Copyright IBM Corporation 2017, 2018

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// runmqserver initializes, creates and starts a queue manager, as PID 1 in a container
package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"

	"github.com/ibm-messaging/mq-container/internal/command"
	"github.com/ibm-messaging/mq-container/internal/name"
)

var debug = false

func logDebug(msg string) {
	if debug {
		log.Debug(msg)
	}
}

func logDebugf(format string, args ...interface{}) {
	if debug {
		log.Debugf(format, args...)
	}
}

// createDirStructure creates the default MQ directory structure under /var/mqm
func createDirStructure() error {
	out, _, err := command.Run("/opt/mqm/bin/crtmqdir", "-f", "-s")
	if err != nil {
		log.Printf("Error creating directory structure: %v\n", string(out))
		return err
	}
	log.Println("Created directory structure under /var/mqm")
	return nil
}

func createQueueManager(name string) error {
	log.Printf("Creating queue manager %v", name)
	out, rc, err := command.Run("crtmqm", "-q", "-p", "1414", name)
	if err != nil {
		// 8=Queue manager exists, which is fine
		if rc != 8 {
			log.Printf("crtmqm returned %v", rc)
			log.Println(string(out))
			return err
		}
		log.Printf("Detected existing queue manager %v", name)
	}
	return nil
}

func updateCommandLevel() error {
	level, ok := os.LookupEnv("MQ_CMDLEVEL")
	if ok && level != "" {
		log.Printf("Setting CMDLEVEL to %v", level)
		out, rc, err := command.Run("strmqm", "-e", "CMDLEVEL="+level)
		if err != nil {
			log.Printf("Error %v setting CMDLEVEL: %v", rc, string(out))
			return err
		}
	}
	return nil
}

func startQueueManager() error {
	log.Println("Starting queue manager")
	out, rc, err := command.Run("strmqm")
	if err != nil {
		log.Printf("Error %v starting queue manager: %v", rc, string(out))
		return err
	}
	log.Println("Started queue manager")
	return nil
}

func configureQueueManager() error {
	const configDir string = "/etc/mqm"
	files, err := ioutil.ReadDir(configDir)
	if err != nil {
		log.Println(err)
		return err
	}

	for _, file := range files {
		if strings.HasSuffix(file.Name(), ".mqsc") {
			abs := filepath.Join(configDir, file.Name())
			mqsc, err := ioutil.ReadFile(abs)
			if err != nil {
				log.Println(err)
				return err
			}
			cmd := exec.Command("runmqsc")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				log.Println(err)
				return err
			}
			stdin.Write(mqsc)
			stdin.Close()
			// Run the command and wait for completion
			out, err := cmd.CombinedOutput()
			if err != nil {
				log.Println(err)
			}
			// Print the runmqsc output, adding tab characters to make it more readable as part of the log
			log.Printf("Output for \"runmqsc\" with %v:\n\t%v", abs, strings.Replace(string(out), "\n", "\n\t", -1))
		}
	}
	return nil
}

func stopQueueManager(name string) error {
	log.Println("Stopping queue manager")
	out, _, err := command.Run("endmqm", "-w", name)
	if err != nil {
		log.Printf("Error stopping queue manager: %v", string(out))
		return err
	}
	log.Println("Stopped queue manager")
	return nil
}

func jsonLogs() bool {
	e := os.Getenv("MQ_ALPHA_JSON_LOGS")
	if e == "true" || e == "1" {
		return true
	}
	return false
}

func mirrorLogs() bool {
	e := os.Getenv("MQ_ALPHA_MIRROR_ERROR_LOGS")
	if e == "true" || e == "1" {
		return true
	}
	return false
}

func configureLogger(name string) {
	if jsonLogs() {
		formatter := logrus.JSONFormatter{
			FieldMap: logrus.FieldMap{
				logrus.FieldKeyMsg:   "message",
				logrus.FieldKeyLevel: "ibm_level",
				logrus.FieldKeyTime:  "ibm_datetime",
			},
			// Match time stamp format used by MQ messages (includes milliseconds)
			TimestampFormat: "2006-01-02T15:04:05.000Z07:00",
		}
		logrus.SetFormatter(&formatter)
	} else {
		formatter := logrus.TextFormatter{
			FullTimestamp: true,
		}
		logrus.SetFormatter(&formatter)
	}
	if debug {
		logrus.SetLevel(logrus.DebugLevel)
	}
}

func doMain() error {
	debugEnv, ok := os.LookupEnv("DEBUG")
	if ok && (debugEnv == "true" || debugEnv == "1") {
		debug = true
	}
	name, err := name.GetQueueManagerName()
	if err != nil {
		log.Println(err)
		return err
	}
	configureLogger(name)
	accepted, err := checkLicense()
	if err != nil {
		return err
	}
	if !accepted {
		return errors.New("License not accepted")
	}
	log.Printf("Using queue manager name: %v", name)

	// Start signal handler
	signalControl := signalHandler(name)

	logConfig()
	err = createVolume("/mnt/mqm")
	if err != nil {
		log.Println(err)
		return err
	}
	err = createDirStructure()
	if err != nil {
		return err
	}
	var mirrorLifecycle chan bool
	if mirrorLogs() {
		f := "/var/mqm/qmgrs/" + name + "/errors/AMQERR01"
		if jsonLogs() {
			f = f + ".json"
			mirrorLifecycle, err = mirrorLog(f, func(msg string) {
				// Print the message straight to stdout
				fmt.Println(msg)
			})
		} else {
			f = f + ".LOG"
			mirrorLifecycle, err = mirrorLog(f, func(msg string) {
				// Log the message, so we get a timestamp etc.
				log.Println(msg)
			})
		}
		if err != nil {
			return err
		}
	}
	err = createQueueManager(name)
	if err != nil {
		return err
	}
	err = updateCommandLevel()
	if err != nil {
		return err
	}
	err = startQueueManager()
	if err != nil {
		return err
	}
	configureQueueManager()
	// Start reaping zombies from now on.
	// Start this here, so that we don't reap any sub-processes created
	// by this process (e.g. for crtmqm or strmqm)
	signalControl <- startReaping
	// Reap zombies now, just in case we've already got some
	signalControl <- reapNow
	// Wait for terminate signal
	<-signalControl
	if mirrorLogs() {
		// Tell the mirroring goroutine to shutdown
		mirrorLifecycle <- true
		// Wait for the mirroring goroutine to finish cleanly
		<-mirrorLifecycle
	}
	return nil
}

var osExit = os.Exit

func main() {
	err := doMain()
	if err != nil {
		osExit(1)
	}
}
