package backends

import (
	"errors"
	"reflect"
	"time"

	"github.com/koblelabs/machinery/v1/tasks"
)

// AsyncResult represents a task result
type AsyncResult struct {
	Signature *tasks.Signature
	taskState *tasks.TaskState
	backend   Interface
}

// ChordAsyncResult represents a result of a chord
type ChordAsyncResult struct {
	groupAsyncResults []*AsyncResult
	chordAsyncResult  *AsyncResult
	backend           Interface
}

// ChainAsyncResult represents a result of a chain of tasks
type ChainAsyncResult struct {
	asyncResults []*AsyncResult
	backend      Interface
}

// NewAsyncResult creates AsyncResult instance
func NewAsyncResult(signature *tasks.Signature, backend Interface) *AsyncResult {
	return &AsyncResult{
		Signature: signature,
		taskState: new(tasks.TaskState),
		backend:   backend,
	}
}

// NewChordAsyncResult creates ChordAsyncResult instance
func NewChordAsyncResult(groupTasks []*tasks.Signature, chordCallback *tasks.Signature, backend Interface) *ChordAsyncResult {
	asyncResults := make([]*AsyncResult, len(groupTasks))
	for i, task := range groupTasks {
		asyncResults[i] = NewAsyncResult(task, backend)
	}
	return &ChordAsyncResult{
		groupAsyncResults: asyncResults,
		chordAsyncResult:  NewAsyncResult(chordCallback, backend),
		backend:           backend,
	}
}

// NewChainAsyncResult creates ChainAsyncResult instance
func NewChainAsyncResult(tasks []*tasks.Signature, backend Interface) *ChainAsyncResult {
	asyncResults := make([]*AsyncResult, len(tasks))
	for i, task := range tasks {
		asyncResults[i] = NewAsyncResult(task, backend)
	}
	return &ChainAsyncResult{
		asyncResults: asyncResults,
		backend:      backend,
	}
}

// Touch the state and don't wait
func (asyncResult *AsyncResult) Touch() ([]reflect.Value, error) {
	if asyncResult.backend == nil {
		return nil, errors.New("Result backend not configured")
	}

	asyncResult.GetState()

	// Purge state if we are using AMQP backend
	_, isAMQPBackend := asyncResult.backend.(*AMQPBackend)
	if isAMQPBackend && asyncResult.taskState.IsCompleted() {
		asyncResult.backend.PurgeState(asyncResult.taskState.TaskUUID)
	}

	if asyncResult.taskState.IsSuccess() {
		resultValues := make([]reflect.Value, len(asyncResult.taskState.Results))
		for i, result := range asyncResult.taskState.Results {
			resultValue, err := tasks.ReflectValue(result.Type, result.Value)
			if err != nil {
				return nil, err
			}
			resultValues[i] = resultValue
		}
		return resultValues, nil
	}

	if asyncResult.taskState.IsFailure() {
		return nil, errors.New(asyncResult.taskState.Error)
	}

	return nil, nil
}

// Get returns task results (synchronous blocking call)
func (asyncResult *AsyncResult) Get(sleepDuration time.Duration) ([]reflect.Value, error) {
	for {
		result, err := asyncResult.Touch()

		if result == nil && err == nil {
			<-time.After(sleepDuration)
		} else {
			return result, err
		}
	}
}

// GetWithTimeout returns task results with a timeout (synchronous blocking call)
func (asyncResult *AsyncResult) GetWithTimeout(timeoutDuration, sleepDuration time.Duration) ([]reflect.Value, error) {
	timeout := time.NewTimer(timeoutDuration)

	for {
		select {
		case <-timeout.C:
			return nil, errors.New("Timeout reached")
		default:
			result, err := asyncResult.Touch()

			if result == nil && err == nil {
				<-time.After(sleepDuration)
			} else {
				return result, err
			}
		}
	}
}

// GetState returns latest task state
func (asyncResult *AsyncResult) GetState() *tasks.TaskState {
	if asyncResult.taskState.IsCompleted() {
		return asyncResult.taskState
	}

	taskState, err := asyncResult.backend.GetState(asyncResult.Signature.UUID)
	if err == nil {
		asyncResult.taskState = taskState
	}

	return asyncResult.taskState
}

// Get returns results of a chain of tasks (synchronous blocking call)
func (chainAsyncResult *ChainAsyncResult) Get(sleepDuration time.Duration) ([]reflect.Value, error) {
	if chainAsyncResult.backend == nil {
		return nil, errors.New("Result backend not configured")
	}

	var (
		results []reflect.Value
		err     error
	)

	for _, asyncResult := range chainAsyncResult.asyncResults {
		results, err = asyncResult.Get(sleepDuration)
		if err != nil {
			return nil, err
		}
	}

	return results, err
}

// Get returns result of a chord (synchronous blocking call)
func (chordAsyncResult *ChordAsyncResult) Get(sleepDuration time.Duration) ([]reflect.Value, error) {
	if chordAsyncResult.backend == nil {
		return nil, errors.New("Result backend not configured")
	}

	var err error
	for _, asyncResult := range chordAsyncResult.groupAsyncResults {
		_, err = asyncResult.Get(sleepDuration)
		if err != nil {
			return nil, err
		}
	}

	return chordAsyncResult.chordAsyncResult.Get(sleepDuration)
}

// GetWithTimeout returns results of a chain of tasks with timeout (synchronous blocking call)
func (chainAsyncResult *ChainAsyncResult) GetWithTimeout(timeoutDuration, sleepDuration time.Duration) ([]reflect.Value, error) {
	if chainAsyncResult.backend == nil {
		return nil, errors.New("Result backend not configured")
	}

	var (
		results []reflect.Value
		err     error
	)

	timeout := time.NewTimer(timeoutDuration)
	ln := len(chainAsyncResult.asyncResults)
	lastResult := chainAsyncResult.asyncResults[ln-1]

	for {
		select {
		case <-timeout.C:
			return nil, errors.New("Timeout reached")
		default:

			for _, asyncResult := range chainAsyncResult.asyncResults {
				_, errcur := asyncResult.Touch()
				if errcur != nil {
					return nil, err
				}
			}

			results, err = lastResult.Touch()
			if err != nil {
				return nil, err
			}
			if results != nil {
				return results, err
			}
			<-time.After(sleepDuration)
		}
	}
}

// GetWithTimeout returns result of a chord with a timeout (synchronous blocking call)
func (chordAsyncResult *ChordAsyncResult) GetWithTimeout(timeoutDuration, sleepDuration time.Duration) ([]reflect.Value, error) {
	if chordAsyncResult.backend == nil {
		return nil, errors.New("Result backend not configured")
	}

	var (
		results []reflect.Value
		err     error
	)

	timeout := time.NewTimer(timeoutDuration)
	for {
		select {
		case <-timeout.C:
			return nil, errors.New("Timeout reached")
		default:
			for _, asyncResult := range chordAsyncResult.groupAsyncResults {
				_, errcur := asyncResult.Touch()
				if errcur != nil {
					return nil, err
				}
			}

			results, err = chordAsyncResult.chordAsyncResult.Touch()
			if err != nil {
				return nil, nil
			}
			if results != nil {
				return results, err
			}
			<-time.After(sleepDuration)
		}
	}
}
