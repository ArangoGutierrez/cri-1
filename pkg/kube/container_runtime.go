// Copyright (c) 2018 Sylabs, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kube

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/sylabs/cri/pkg/image"
	"github.com/sylabs/cri/pkg/singularity/runtime"
	"github.com/sylabs/singularity/pkg/ociruntime"
)

func (c *Container) spawnOCIContainer(imgInfo *image.Info) error {
	err := c.addOCIBundle(imgInfo)
	if err != nil {
		return fmt.Errorf("could not create oci bundle: %v", err)
	}

	syncCtx, cancel := context.WithCancel(context.Background())
	c.syncCancel = cancel
	c.syncChan, err = runtime.ObserveState(syncCtx, c.socketPath())
	if err != nil {
		return fmt.Errorf("could not listen for state changes: %v", err)
	}

	go c.cli.Create(c.id, c.bundlePath(), "--sync-socket", c.socketPath(), "--log-path", c.logPath)

	if err := c.expectState(runtime.StateCreating); err != nil {
		return err
	}
	if err := c.expectState(runtime.StateCreated); err != nil {
		return err
	}

	return nil
}

// UpdateState updates container state according to information
// received from the runtime.
func (c *Container) UpdateState() error {
	contState, err := c.cli.State(c.id)
	if err != nil {
		return fmt.Errorf("could not get container state: %v", err)
	}

	c.createdAt, err = parseIntAnnotation(contState.Annotations[ociruntime.AnnotationCreatedAt])
	if err != nil {
		return fmt.Errorf("could not parse created timestamp: %v", err)
	}
	c.startedAt, err = parseIntAnnotation(contState.Annotations[ociruntime.AnnotationStartedAt])
	if err != nil {
		return fmt.Errorf("could not parse started timestamp: %v", err)
	}
	c.finishedAt, err = parseIntAnnotation(contState.Annotations[ociruntime.AnnotationFinishedAt])
	if err != nil {
		return fmt.Errorf("could not parse finished timestamp: %v", err)
	}
	exitCode, err := parseIntAnnotation(contState.Annotations[ociruntime.AnnotationExitCode])
	if err != nil {
		return fmt.Errorf("could not parse exit code: %v", err)
	}
	c.exitCode = int32(exitCode)
	c.runtimeState = runtime.StatusToState(contState.Status)
	c.exitDesc = contState.Annotations[ociruntime.AnnotationExitDesc]
	c.attachSocket = contState.Annotations[ociruntime.AnnotationAttachSocket]
	c.controlSocket = contState.Annotations[ociruntime.AnnotationControlSocket]

	return nil
}

func (c *Container) expectState(expect runtime.State) error {
	log.Printf("waiting for state %d...", expect)
	c.runtimeState = <-c.syncChan
	if c.runtimeState != expect {
		return fmt.Errorf("unexpected container state: %v", c.runtimeState)
	}
	return nil
}

func (c *Container) terminate(timeout int64) error {
	// Call cancel to free any resources taken by context.
	// We should call it when sync socket will no longer be used, and
	// since multiple calls are fine with cancel func, call it at
	// the end of terminate.
	defer c.syncCancel()

	if c.runtimeState == runtime.StateExited {
		return nil
	}

	if timeout == 0 { // if timeout is 0, forcibly remove process
		return c.kill()
	}

	// otherwise give container a chance to terminate gracefully
	err := c.cli.Kill(c.id, false)
	if err != nil {
		return fmt.Errorf("could not treminate container: %v", err)
	}
	select {
	case c.runtimeState = <-c.syncChan:
		if c.runtimeState != runtime.StateExited {
			return fmt.Errorf("unexpected container state: %v", c.runtimeState)
		}
	case <-time.After(time.Second * time.Duration(timeout)):
		return c.kill()
	}

	return nil
}

func (c *Container) kill() error {
	// Call cancel to free any resources taken by context.
	// We should call it when sync socket will no longer be used, and
	// since multiple calls are fine with cancel func, call it at
	// the end of kill.
	defer c.syncCancel()

	if c.runtimeState == runtime.StateExited {
		return nil
	}

	err := c.cli.Kill(c.id, true)
	if err != nil {
		return fmt.Errorf("could not kill container: %v", err)
	}
	return c.expectState(runtime.StateExited)
}

func parseIntAnnotation(ts string) (int64, error) {
	if ts == "" {
		return 0, nil
	}
	return strconv.ParseInt(ts, 10, 64)
}
