package directrequestocr

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/smartcontractkit/chainlink/core/chains/evm/log"
	"github.com/smartcontractkit/chainlink/core/gethwrappers/generated/ocr2dr_oracle"
	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/services/job"
	"github.com/smartcontractkit/chainlink/core/services/ocr2/plugins/directrequestocr/config"
	"github.com/smartcontractkit/chainlink/core/services/pg"
	"github.com/smartcontractkit/chainlink/core/services/pipeline"
	"github.com/smartcontractkit/chainlink/core/utils"
)

var (
	_ log.Listener   = &DRListener{}
	_ job.ServiceCtx = &DRListener{}
)

const (
	ParseResultTaskName string = "parse_result"
	ParseErrorTaskName  string = "parse_error"
)

type DRListener struct {
	utils.StartStopOnce
	oracle            *ocr2dr_oracle.OCR2DROracle
	job               job.Job
	pipelineRunner    pipeline.Runner
	jobORM            job.ORM
	logBroadcaster    log.Broadcaster
	shutdownWaitGroup sync.WaitGroup
	mbOracleEvents    *utils.Mailbox[log.Broadcast]
	serviceContext    context.Context
	serviceCancel     context.CancelFunc
	chStop            chan struct{}
	pluginORM         ORM
	pluginConfig      config.PluginConfig
	logger            logger.Logger
	mailMon           *utils.MailboxMonitor
}

func NewDRListener(oracle *ocr2dr_oracle.OCR2DROracle, jb job.Job, runner pipeline.Runner, jobORM job.ORM, pluginORM ORM, pluginConfig config.PluginConfig, logBroadcaster log.Broadcaster, lggr logger.Logger, mailMon *utils.MailboxMonitor) *DRListener {
	return &DRListener{
		oracle:         oracle,
		job:            jb,
		pipelineRunner: runner,
		jobORM:         jobORM,
		logBroadcaster: logBroadcaster,
		mbOracleEvents: utils.NewHighCapacityMailbox[log.Broadcast](),
		chStop:         make(chan struct{}),
		pluginORM:      pluginORM,
		pluginConfig:   pluginConfig,
		logger:         lggr,
		mailMon:        mailMon,
	}
}

// Start complies with job.Service
func (l *DRListener) Start(context.Context) error {
	return l.StartOnce("DirectRequestListener", func() error {
		l.serviceContext, l.serviceCancel = context.WithCancel(context.Background())
		unsubscribeLogs := l.logBroadcaster.Register(l, log.ListenerOpts{
			Contract: l.oracle.Address(),
			ParseLog: l.oracle.ParseLog,
			LogsWithTopics: map[common.Hash][][]log.Topic{
				ocr2dr_oracle.OCR2DROracleOracleRequest{}.Topic():  {},
				ocr2dr_oracle.OCR2DROracleOracleResponse{}.Topic(): {},
			},
			MinIncomingConfirmations: l.pluginConfig.MinIncomingConfirmations,
		})
		l.shutdownWaitGroup.Add(3)
		go l.processOracleEvents()
		go l.timeoutRequests()
		go func() {
			<-l.chStop
			unsubscribeLogs()
			l.shutdownWaitGroup.Done()
		}()

		l.mailMon.Monitor(l.mbOracleEvents, "DirectRequestListener", "OracleEvents", fmt.Sprint(l.job.ID))

		return nil
	})
}

// Close complies with job.Service
func (l *DRListener) Close() error {
	return l.StopOnce("DirectRequestListener", func() error {
		l.serviceCancel()
		close(l.chStop)
		l.shutdownWaitGroup.Wait()

		return l.mbOracleEvents.Close()
	})
}

// HandleLog implements log.Listener
func (l *DRListener) HandleLog(lb log.Broadcast) {
	log := lb.DecodedLog()
	if log == nil || reflect.ValueOf(log).IsNil() {
		l.logger.Error("HandleLog: ignoring nil value")
		return
	}

	switch log := log.(type) {
	case *ocr2dr_oracle.OCR2DROracleOracleRequest, *ocr2dr_oracle.OCR2DROracleOracleResponse:
		wasOverCapacity := l.mbOracleEvents.Deliver(lb)
		if wasOverCapacity {
			l.logger.Error("OracleRequest log mailbox is over capacity - dropped the oldest log")
		}
	default:
		l.logger.Errorf("Unexpected log type %T", log)
	}
}

// JobID() complies with log.Listener
func (l *DRListener) JobID() int32 {
	return l.job.ID
}

func (l *DRListener) processOracleEvents() {
	defer l.shutdownWaitGroup.Done()
	for {
		select {
		case <-l.chStop:
			return
		case <-l.mbOracleEvents.Notify():
			for {
				select {
				case <-l.chStop:
					return
				default:
				}
				lb, exists := l.mbOracleEvents.Retrieve()
				if !exists {
					break
				}
				was, err := l.logBroadcaster.WasAlreadyConsumed(lb)
				if err != nil {
					l.logger.Errorw("Could not determine if log was already consumed", "error", err)
					continue
				} else if was {
					continue
				}

				log := lb.DecodedLog()
				if log == nil || reflect.ValueOf(log).IsNil() {
					l.logger.Error("processOracleEvents: ignoring nil value")
					continue
				}

				switch log := log.(type) {
				case *ocr2dr_oracle.OCR2DROracleOracleRequest:
					l.shutdownWaitGroup.Add(1)
					go l.handleOracleRequest(log, lb)
				case *ocr2dr_oracle.OCR2DROracleOracleResponse:
					l.shutdownWaitGroup.Add(1)
					go l.handleOracleResponse(log, lb)
				default:
					l.logger.Warnf("Unexpected log type %T", log)
				}
			}
		}
	}
}

// Process result from the EA saved by a jsonparse pipeline task.
// That value is a valid JSON string so it contains double quote characters.
// Allowed inputs are:
//
//  1. "" (2 characters) -> return empty byte array
//  2. "0x<val>" where <val> is a non-empty, valid hex -> return hex-decoded <val>
func ExtractRawBytes(input []byte) ([]byte, error) {
	if len(input) < 2 || input[0] != '"' || input[len(input)-1] != '"' {
		return nil, fmt.Errorf("unable to decode input: %v", input)
	}
	input = input[1 : len(input)-1]
	if len(input) == 0 {
		return []byte{}, nil
	}
	if len(input) < 4 || len(input)%2 != 0 {
		return nil, fmt.Errorf("input is not a valid, non-empty hex string of even length: %v", input)
	}
	return utils.TryParseHex(string(input))
}

func (l *DRListener) handleOracleRequest(request *ocr2dr_oracle.OCR2DROracleOracleRequest, lb log.Broadcast) {
	defer l.shutdownWaitGroup.Done()
	l.logger.Infow("Oracle request received",
		"requestId", fmt.Sprintf("%0x", request.RequestId),
		"data", fmt.Sprintf("%0x", request.Data),
	)

	requestData := make(map[string]interface{})
	requestData["requestId"] = formatRequestId(request.RequestId)
	requestData["data"] = fmt.Sprintf("0x%x", request.Data)
	meta := make(map[string]interface{})
	meta["oracleRequest"] = requestData

	vars := pipeline.NewVarsFrom(map[string]interface{}{
		"jobSpec": map[string]interface{}{
			"databaseID":    l.job.ID,
			"externalJobID": l.job.ExternalJobID,
			"name":          l.job.Name.ValueOrZero(),
		},
		"jobRun": map[string]interface{}{
			"meta":                  meta,
			"logBlockHash":          request.Raw.BlockHash,
			"logBlockNumber":        request.Raw.BlockNumber,
			"logTxHash":             request.Raw.TxHash,
			"logAddress":            request.Raw.Address,
			"logTopics":             request.Raw.Topics,
			"logData":               request.Raw.Data,
			"blockReceiptsRoot":     lb.ReceiptsRoot(),
			"blockTransactionsRoot": lb.TransactionsRoot(),
			"blockStateRoot":        lb.StateRoot(),
		},
	})
	run := pipeline.NewRun(*l.job.PipelineSpec, vars)
	err := l.pluginORM.CreateRequest(request.RequestId, time.Now(), &request.Raw.TxHash)
	if err != nil {
		l.logger.Errorf("Failed to create a DB entry for new request (ID: %v)", request.RequestId)
		return
	}
	_, err = l.pipelineRunner.Run(l.serviceContext, &run, l.logger, true, func(tx pg.Queryer) error {
		l.markLogConsumed(lb, pg.WithQueryer(tx))
		return nil
	})
	if err != nil {
		l.logger.Errorf("Pipeline run failed for request ID: %v, err: %s", request.RequestId, err)
		return
	}

	computationResult, errResult := l.jobORM.FindTaskResultByRunIDAndTaskName(run.ID, ParseResultTaskName)
	if errResult != nil {
		// Internal problem: Can't find parsed computation results
		if err2 := l.pluginORM.SetError(request.RequestId, run.ID, NODE_EXCEPTION, []byte(errResult.Error()), time.Now()); err2 != nil {
			l.logger.Errorf("Call to SetError failed for request ID: %v", request.RequestId)
		}
		return
	}
	computationResult, errResult = ExtractRawBytes(computationResult)
	if errResult != nil {
		l.logger.Errorf("Failed to extract result for request ID: %v, err: %s", request.RequestId, errResult)
		return
	}

	computationError, errErr := l.jobORM.FindTaskResultByRunIDAndTaskName(run.ID, ParseErrorTaskName)
	if errErr != nil {
		// Internal problem: Can't find parsed computation error
		if err2 := l.pluginORM.SetError(request.RequestId, run.ID, NODE_EXCEPTION, []byte(errErr.Error()), time.Now()); err2 != nil {
			l.logger.Errorf("Call to SetError failed for request ID: %v", request.RequestId)
		}
		return
	}
	computationError, errErr = ExtractRawBytes(computationError)
	if errErr != nil {
		l.logger.Errorf("Failed to extract error for request ID: %v, err: %s", request.RequestId, errErr)
		return
	}

	if len(computationError) != 0 {
		if err2 := l.pluginORM.SetError(request.RequestId, run.ID, USER_EXCEPTION, computationError, time.Now()); err2 != nil {
			l.logger.Errorf("Call to SetError failed for request ID: %v", request.RequestId)
		}
	} else {
		if err2 := l.pluginORM.SetResult(request.RequestId, run.ID, computationResult, time.Now()); err2 != nil {
			l.logger.Errorf("Call to SetResult failed for request ID: %v", request.RequestId)
		}
	}
}

func (l *DRListener) handleOracleResponse(response *ocr2dr_oracle.OCR2DROracleOracleResponse, lb log.Broadcast) {
	defer l.shutdownWaitGroup.Done()
	l.logger.Infow("Oracle response received", "requestId", fmt.Sprintf("%0x", response.RequestId))

	if err := l.pluginORM.SetConfirmed(response.RequestId); err != nil {
		l.logger.Errorf("Setting CONFIRMED state failed for request ID: %v", response.RequestId)
	}
}

func (l *DRListener) markLogConsumed(lb log.Broadcast, qopts ...pg.QOpt) {
	if err := l.logBroadcaster.MarkConsumed(lb, qopts...); err != nil {
		l.logger.Errorw("Unable to mark log consumed", "err", err, "log", lb.String())
	}
}

func formatRequestId(requestId [32]byte) string {
	return fmt.Sprintf("0x%x", requestId)
}

func (l *DRListener) timeoutRequests() {
	defer l.shutdownWaitGroup.Done()
	timeoutSec, freqSec, batchSize := l.pluginConfig.RequestTimeoutSec, l.pluginConfig.RequestTimeoutCheckFrequencySec, l.pluginConfig.RequestTimeoutBatchLookupSize
	if timeoutSec == 0 || freqSec == 0 || batchSize == 0 {
		l.logger.Warn("request timeout checker not configured - disabling it")
		return
	}
	ticker := time.NewTicker(time.Duration(freqSec) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-l.chStop:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-(time.Duration(timeoutSec) * time.Second))
			ids, err := l.pluginORM.TimeoutExpiredResults(cutoff, batchSize)
			if err != nil {
				l.logger.Errorw("error when calling FindExpiredResults", "err", err)
				break
			}
			if len(ids) > 0 {
				l.logger.Debugw("timed out requests", "ids", ids)
			} else {
				l.logger.Debug("no requests to time out")
			}
		}
	}
}
