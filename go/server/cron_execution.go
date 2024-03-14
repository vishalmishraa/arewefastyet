/*
 *
 * Copyright 2021 The Vitess Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 * /
 */

package server

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/vitessio/arewefastyet/go/exec"
)

func (s *Server) executeSingle(config benchmarkConfig, identifier executionIdentifier) (err error) {
	var e *exec.Exec
	defer func() {
		if e != nil {
			if err != nil {
				err = fmt.Errorf("%v", err)
			}
			if errSuccess := e.Success(); errSuccess != nil {
				err = errSuccess
				return
			}
			slog.Info("Finished execution: UUID: [", e.UUID.String(), "], Git Ref: [", identifier.GitRef, "], Type: [", identifier.BenchmarkType, "]")
		}
	}()

	e, err = exec.NewExecWithConfig(config.file, identifier.UUID)

	if err != nil {
		nErr := fmt.Errorf(fmt.Sprintf("new exec error: %v", err))
		slog.Error(nErr.Error())
		return nErr
	}
	e.Source = identifier.Source
	e.GitRef = identifier.GitRef
	e.VtgatePlannerVersion = identifier.PlannerVersion
	e.PullNB = identifier.PullNb
	e.PullBaseBranchRef = identifier.PullBaseRef
	e.VitessVersion = identifier.Version
	e.RepoDir = s.getVitessPath()

	slog.Info("Starting execution: UUID: [", e.UUID.String(), "], Git Ref: [", identifier.GitRef, "], Type: [", identifier.BenchmarkType, "]")
	err = e.Prepare()
	if err != nil {
		nErr := fmt.Errorf(fmt.Sprintf("prepare error: %v", err))
		slog.Error(nErr.Error())
		return nErr
	}

	err = e.SetOutputToDefaultPath()
	if err != nil {
		nErr := fmt.Errorf(fmt.Sprintf("prepare output error: %v", err))
		slog.Error(nErr.Error())
		return nErr
	}

	timeout := 1 * time.Hour
	if identifier.BenchmarkType == "micro" {
		timeout = 4 * time.Hour
	}
	err = e.ExecuteWithTimeout(timeout)
	if err != nil {
		nErr := fmt.Errorf(fmt.Sprintf("execute with timeout error: %v", err))
		slog.Error(nErr.Error())
		return nErr
	}
	return nil
}

func (s *Server) executeElement(element *executionQueueElement) {
	if element.retry < 0 {
		if _, found := queue[element.identifier]; found {
			// removing the element from the queue since we are done with it
			mtx.Lock()
			delete(queue, element.identifier)
			mtx.Unlock()
		}
		decrementNumberOfOnGoingExecution()
		return
	}

	// execute with the given configuration file and exec identifier
	err := s.executeSingle(element.config, element.identifier)
	if err != nil {
		slog.Error(err.Error())

		// execution failed, we retry
		element.retry -= 1
		element.identifier.UUID = uuid.NewString()
		s.executeElement(element)
		return
	}

	go func() {
		// removing the element from the queue since we are done with it
		mtx.Lock()
		delete(queue, element.identifier)
		mtx.Unlock()

		// we will wait for the benchmarks we need to compare it against and notify users if needed
		s.compareElement(element)
	}()

	decrementNumberOfOnGoingExecution()
}

func (s *Server) compareElement(element *executionQueueElement) {
	// map that contains all the comparison we saw and analyzed
	seen := map[executionIdentifier]bool{}
	done := 0
	for done != len(element.compareWith) {
		time.Sleep(1 * time.Second)
		for _, comparer := range element.compareWith {
			// checking if we have already seen this comparison, if we did, we can skip it.
			if _, ok := seen[comparer]; ok {
				continue
			}
			comparerUUID, err := exec.GetFinishedExecution(s.dbClient, comparer.GitRef, comparer.Source, comparer.BenchmarkType, comparer.PlannerVersion, comparer.PullNb)
			if err != nil {
				slog.Error(err)
				return
			}
			if comparerUUID != "" {
				err := s.sendNotificationForRegression(
					element.identifier.Source,
					comparer.Source,
					element.identifier.GitRef,
					comparer.GitRef,
					element.identifier.PlannerVersion,
					element.identifier.BenchmarkType,
					element.identifier.PullNb,
					element.notifyAlways,
				)
				if err != nil {
					slog.Error(err)
					return
				}
				seen[comparer] = true
				done++
			}
		}
	}
}

func (s *Server) getNumberOfBenchmarksInDB(identifier executionIdentifier) (int, error) {
	var nb int
	var err error
	if identifier.BenchmarkType == "micro" {
		var exists bool
		exists, err = exec.Exists(s.dbClient, identifier.GitRef, identifier.Source, identifier.BenchmarkType, exec.StatusFinished)
		if exists {
			nb = 1
		}
	} else {
		nb, err = exec.CountMacroBenchmark(s.dbClient, identifier.GitRef, identifier.Source, identifier.BenchmarkType, exec.StatusFinished, identifier.PlannerVersion)
	}
	if err != nil {
		slog.Error(err)
		return 0, err
	}
	return nb, nil
}

func (s *Server) cronExecutionQueueWatcher() {
	var lastExecutedId executionIdentifier

	queueWatch := func() {
		mtx.Lock()
		defer mtx.Unlock()
		if currentCountExec >= maxConcurJob {
			return
		}

		// Prioritize executing the same configuration of benchmark in a row
		var nextExecuteElement *executionQueueElement
		for _, element := range queue {
			if element.Executing {
				continue
			}
			if element.identifier.equalWithoutUUID(lastExecutedId) {
				nextExecuteElement = element
				break
			}
		}

		// If we did not find any matching element just go to the first one which is not executing
		if nextExecuteElement == nil {
			for _, element := range queue {
				if !element.Executing {
					nextExecuteElement = element
					break
				}
			}
		}

		// Execute the element if found
		if nextExecuteElement != nil {
			currentCountExec++
			lastExecutedId = nextExecuteElement.identifier

			// setting this element to `Executing = true`, so we do not execute it twice in the future
			nextExecuteElement.Executing = true
			go s.executeElement(nextExecuteElement)
			return
		}
	}

	for {
		queueWatch()
	}
}

func decrementNumberOfOnGoingExecution() {
	mtx.Lock()
	currentCountExec--
	mtx.Unlock()
}
