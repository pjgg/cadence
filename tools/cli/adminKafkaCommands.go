// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"regexp"
	"strings"
	"sync/atomic"

	"time"

	"strconv"

	"runtime"

	"github.com/Shopify/sarama"
	"github.com/bsm/sarama-cluster"
	"github.com/gocql/gocql"
	"github.com/uber-common/bark"
	"github.com/uber/cadence/.gen/go/replicator"
	"github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/messaging"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/persistence/cassandra"
	"github.com/uber/cadence/service/history"
	"github.com/urfave/cli"
	"go.uber.org/thriftrw/protocol"
	"go.uber.org/thriftrw/wire"
	"gopkg.in/yaml.v2"
)

type filterFn func(*replicator.ReplicationTask) bool

const (
	bufferSize                 = 4096
	preambleVersion0      byte = 0x59
	malformedMessage           = "Input was malformed"
	chanBufferSize             = 10000
	maxRereplicateEventID      = 999999
)

var (
	r = regexp.MustCompile(`Partition: .*?, Offset: .*?, Key: .*?`)
)

// AdminKafkaParse parses the output of k8read and outputs replication tasks
func AdminKafkaParse(c *cli.Context) {
	inputFile := getInputFile(c.String(FlagInputFile))
	outputFile := getOutputFile(c.String(FlagOutputFilename))
	filter := buildFilterFn(c.String(FlagWorkflowID), c.String(FlagRunID))

	defer inputFile.Close()
	defer outputFile.Close()

	readerCh := make(chan []byte, chanBufferSize)
	writerCh := make(chan *replicator.ReplicationTask, chanBufferSize)
	doneCh := make(chan struct{})

	var skippedCount int32
	skipErrMode := c.Bool(FlagSkipErrorMode)
	headerMode := c.Bool(FlagHeadersMode)

	go startReader(inputFile, readerCh)
	go startParser(readerCh, writerCh, skipErrMode, &skippedCount)
	go startWriter(outputFile, writerCh, filter, doneCh, skipErrMode, &skippedCount, headerMode)

	<-doneCh

	if skipErrMode {
		fmt.Printf("%v messages were skipped due to errors in parsing", atomic.LoadInt32(&skippedCount))
	}
}

func buildFilterFn(workflowID, runID string) filterFn {
	return func(task *replicator.ReplicationTask) bool {
		if len(workflowID) != 0 && *task.HistoryTaskAttributes.WorkflowId != workflowID {
			return false
		}
		if len(runID) != 0 && *task.HistoryTaskAttributes.RunId != runID {
			return false
		}
		return true
	}
}

func getInputFile(inputFile string) *os.File {
	if len(inputFile) == 0 {
		info, err := os.Stdin.Stat()
		if err != nil {
			ErrorAndExit("Failed to stat stdin file handle", err)
		}
		if info.Mode()&os.ModeCharDevice != 0 || info.Size() <= 0 {
			fmt.Println("Misuse of pipe mode")
			os.Exit(1)
		}
		return os.Stdin
	}
	f, err := os.Open(inputFile)
	if err != nil {
		ErrorAndExit(fmt.Sprintf("Failed to open input file for reading: %v", inputFile), err)
	}
	return f
}

func getOutputFile(outputFile string) *os.File {
	if len(outputFile) == 0 {
		return os.Stdout
	}
	f, err := os.Create(outputFile)
	if err != nil {
		ErrorAndExit("failed to create output file", err)
	}
	return f
}

func startReader(file *os.File, readerCh chan<- []byte) {
	defer close(readerCh)
	reader := bufio.NewReader(file)

	for {
		buf := make([]byte, bufferSize)
		n, err := reader.Read(buf)
		if err != nil {
			if err != io.EOF {
				ErrorAndExit("Failed to read from reader", err)
			} else {
				break
			}

		}
		buf = buf[:n]
		readerCh <- buf
	}
}

func startParser(readerCh <-chan []byte, writerCh chan<- *replicator.ReplicationTask, skipErrors bool, skippedCount *int32) {
	defer close(writerCh)

	var buffer []byte
Loop:
	for {
		select {
		case data, ok := <-readerCh:
			if !ok {
				break Loop
			}
			buffer = append(buffer, data...)
			data, nextBuffer := splitBuffer(buffer)
			buffer = nextBuffer
			parse(data, skipErrors, skippedCount, writerCh)
		}
	}
	parse(buffer, skipErrors, skippedCount, writerCh)
}

func startWriter(
	output *os.File,
	writerCh <-chan *replicator.ReplicationTask,
	filter filterFn,
	doneCh chan struct{},
	skipErrors bool,
	skippedCount *int32,
	headerMode bool,
) {

	defer close(doneCh)

Loop:
	for {
		select {
		case task, ok := <-writerCh:
			if !ok {
				break Loop
			}
			if filter(task) {
				jsonStr, err := json.Marshal(task)
				if err != nil {
					if !skipErrors {
						ErrorAndExit(malformedMessage, fmt.Errorf("failed to encode into json, err: %v", err))
					} else {
						atomic.AddInt32(skippedCount, 1)
						continue Loop
					}
				}

				var outStr string
				if !headerMode {
					outStr = string(jsonStr)
				} else {
					outStr = fmt.Sprintf(
						"%v, %v, %v, %v, %v",
						*task.HistoryTaskAttributes.DomainId,
						*task.HistoryTaskAttributes.WorkflowId,
						*task.HistoryTaskAttributes.RunId,
						*task.HistoryTaskAttributes.FirstEventId,
						*task.HistoryTaskAttributes.NextEventId,
					)
				}
				output.WriteString(fmt.Sprintf("%v\n", outStr))
			}
		}
	}
}

func splitBuffer(buffer []byte) ([]byte, []byte) {
	matches := r.FindAllIndex(buffer, -1)
	if len(matches) == 0 {
		ErrorAndExit(malformedMessage, errors.New("header not found, did you generate dump with -v"))
	}
	splitIndex := matches[len(matches)-1][0]
	return buffer[:splitIndex], buffer[splitIndex:]
}

func parse(bytes []byte, skipErrors bool, skippedCount *int32, writerCh chan<- *replicator.ReplicationTask) {
	messages, skippedGetMsgCount := getMessages(bytes, skipErrors)
	tasks, skippedDeserializeCount := deserializeMessages(messages, skipErrors)
	atomic.AddInt32(skippedCount, skippedGetMsgCount+skippedDeserializeCount)
	for _, t := range tasks {
		writerCh <- t
	}
}

func getMessages(data []byte, skipErrors bool) ([][]byte, int32) {
	str := string(data)
	messagesWithHeaders := r.Split(str, -1)
	if len(messagesWithHeaders[0]) != 0 {
		ErrorAndExit(malformedMessage, errors.New("got data chunk to handle that does not start with valid header"))
	}
	messagesWithHeaders = messagesWithHeaders[1:]
	var rawMessages [][]byte
	var skipped int32
	for _, m := range messagesWithHeaders {
		if len(m) == 0 {
			ErrorAndExit(malformedMessage, errors.New("got empty message between valid headers"))
		}
		curr := []byte(m)
		messageStart := bytes.Index(curr, []byte{preambleVersion0})
		if messageStart == -1 {
			if !skipErrors {
				ErrorAndExit(malformedMessage, errors.New("failed to find message preamble"))
			} else {
				skipped++
				continue
			}
		}
		rawMessages = append(rawMessages, curr[messageStart:])
	}
	return rawMessages, skipped
}

func deserializeMessages(messages [][]byte, skipErrors bool) ([]*replicator.ReplicationTask, int32) {
	var replicationTasks []*replicator.ReplicationTask
	var skipped int32
	for _, m := range messages {
		var task replicator.ReplicationTask
		err := decode(m, &task)
		if err != nil {
			if !skipErrors {
				ErrorAndExit(malformedMessage, err)
			} else {
				skipped++
				continue
			}
		}
		replicationTasks = append(replicationTasks, &task)
	}
	return replicationTasks, skipped
}

func decode(message []byte, val *replicator.ReplicationTask) error {
	reader := bytes.NewReader(message[1:])
	wireVal, err := protocol.Binary.Decode(reader, wire.TStruct)
	if err != nil {
		return err
	}
	return val.FromWire(wireVal)
}

// ClustersConfig describes the kafka clusters
type ClustersConfig struct {
	Clusters map[string]messaging.ClusterConfig
}

func doRereplicate(shardID int, domainID, wid, rid string, minID, maxID int64, targets []string, producer messaging.Producer, session *gocql.Session) {
	if minID <= 0 {
		minID = 1
	}
	if maxID == 0 {
		maxID = maxRereplicateEventID
	}

	histV1 := cassandra.NewHistoryPersistenceFromSession(session, bark.NewNopLogger())
	historyMgr := persistence.NewHistoryManagerImpl(histV1, bark.NewNopLogger())

	histV2 := cassandra.NewHistoryV2PersistenceFromSession(session, bark.NewNopLogger())
	historyV2Mgr := persistence.NewHistoryV2ManagerImpl(histV2, bark.NewNopLogger())

	exeM := cassandra.NewWorkflowExecutionPersistenceFromSession(session, shardID, bark.NewNopLogger())
	exeMgr := persistence.NewExecutionManagerImpl(exeM, bark.NewNopLogger())

	for {
		fmt.Printf("Start rereplicate for wid: %v, rid:%v \n", wid, rid)
		resp, err := exeMgr.GetWorkflowExecution(&persistence.GetWorkflowExecutionRequest{
			DomainID: domainID,
			Execution: shared.WorkflowExecution{
				WorkflowId: common.StringPtr(wid),
				RunId:      common.StringPtr(rid),
			},
		})
		if err != nil {
			ErrorAndExit("GetWorkflowExecution error", err)
		}

		currVersion := resp.State.ReplicationState.CurrentVersion
		repInfo := map[string]*persistence.ReplicationInfo{
			"": {
				Version:     currVersion,
				LastEventID: 0,
			},
		}

		exeInfo := resp.State.ExecutionInfo
		taskTemplate := &persistence.ReplicationTaskInfo{
			DomainID:            domainID,
			WorkflowID:          wid,
			RunID:               rid,
			Version:             currVersion,
			LastReplicationInfo: repInfo,
			EventStoreVersion:   exeInfo.EventStoreVersion,
			BranchToken:         exeInfo.GetCurrentBranch(),
		}

		_, historyBatches, err := history.GetAllHistory(historyMgr, historyV2Mgr, nil, bark.NewNopLogger(), true,
			domainID, wid, rid, minID, maxID, exeInfo.EventStoreVersion, exeInfo.GetCurrentBranch())

		if err != nil {
			ErrorAndExit("GetAllHistory error", err)
		}

		continueAsNew := false
		var newRunID string
		for _, batch := range historyBatches {

			events := batch.Events
			firstEvent := events[0]
			lastEvent := events[len(events)-1]
			if lastEvent.GetEventType() == shared.EventTypeWorkflowExecutionContinuedAsNew {
				continueAsNew = true
				newRunID = lastEvent.WorkflowExecutionContinuedAsNewEventAttributes.GetNewExecutionRunId()
				resp, err := exeMgr.GetWorkflowExecution(&persistence.GetWorkflowExecutionRequest{
					DomainID: domainID,
					Execution: shared.WorkflowExecution{
						WorkflowId: common.StringPtr(wid),
						RunId:      common.StringPtr(newRunID),
					},
				})
				if err != nil {
					ErrorAndExit("GetWorkflowExecution error", err)
				}
				taskTemplate.NewRunEventStoreVersion = resp.State.ExecutionInfo.EventStoreVersion
				taskTemplate.NewRunBranchToken = resp.State.ExecutionInfo.GetCurrentBranch()
			}

			taskTemplate.FirstEventID = firstEvent.GetEventId()
			taskTemplate.NextEventID = lastEvent.GetEventId() + 1
			task, err := history.GenerateReplicationTask(targets, taskTemplate, historyMgr, historyV2Mgr, nil, bark.NewNopLogger(), batch)
			if err != nil {
				ErrorAndExit("GenerateReplicationTask error", err)
			}
			err = producer.Publish(task)
			if err != nil {
				ErrorAndExit("Publish task error", err)
			}
			fmt.Printf("publish task successfully firstEventID %v, lastEventID %v \n", firstEvent.GetEventId(), lastEvent.GetEventId())
		}

		fmt.Printf("Done rereplicate for wid: %v, rid:%v \n", wid, rid)
		runtime.GC()
		if continueAsNew {
			rid = newRunID
			minID = 1
			maxID = maxRereplicateEventID
		} else {
			break
		}
	}
}

// AdminRereplicate parses will re-publish replication tasks to topic
func AdminRereplicate(c *cli.Context) {
	numberOfShards := c.Int(FlagNumberOfShards)
	if numberOfShards <= 0 {
		ErrorAndExit("numberOfShards is must be > 0", nil)
		return
	}
	target := getRequiredOption(c, FlagTargetCluster)
	targets := []string{target}

	producer := newKafkaProducer(c)
	session := connectToCassandra(c)

	if c.IsSet(FlagInputFile) {
		inFile := c.String(FlagInputFile)
		// parse domainID,workflowID,runID,minEventID,maxEventID
		file, err := os.Open(inFile)
		if err != nil {
			ErrorAndExit("Open failed", err)
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		idx := 0
		for scanner.Scan() {
			idx++
			line := strings.TrimSpace(scanner.Text())
			if len(line) == 0 {
				fmt.Printf("line %v is empty, skipped\n", idx)
				continue
			}
			cols := strings.Split(line, ",")
			if len(cols) < 3 {
				ErrorAndExit("Split failed", fmt.Errorf("line %v has less than 3 cols separated by comma, only %v ", idx, len(cols)))
			}
			fmt.Printf("Start processing line %v ...\n", idx)
			domainID := strings.TrimSpace(cols[0])
			wid := strings.TrimSpace(cols[1])
			rid := strings.TrimSpace(cols[2])
			var minID, maxID int64
			if len(cols) >= 4 {
				i, err := strconv.Atoi(strings.TrimSpace(cols[3]))
				if err != nil {
					ErrorAndExit(fmt.Sprintf("Atoi failed at lne %v", idx), err)
				}
				minID = int64(i)
			}
			if len(cols) >= 5 {
				i, err := strconv.Atoi(strings.TrimSpace(cols[4]))
				if err != nil {
					ErrorAndExit(fmt.Sprintf("Atoi failed at lne %v", idx), err)
				}
				maxID = int64(i)
			}

			shardID := common.WorkflowIDToHistoryShard(wid, numberOfShards)
			doRereplicate(shardID, domainID, wid, rid, minID, maxID, targets, producer, session)
			fmt.Printf("Done processing line %v ...\n", idx)
		}
		if err := scanner.Err(); err != nil {
			ErrorAndExit("scanner failed", err)
		}
	} else {
		domainID := getRequiredOption(c, FlagDomainID)
		wid := getRequiredOption(c, FlagWorkflowID)
		rid := getRequiredOption(c, FlagRunID)
		minID := c.Int64(FlagMinEventID)
		maxID := c.Int64(FlagMaxEventID)

		shardID := common.WorkflowIDToHistoryShard(wid, numberOfShards)
		doRereplicate(shardID, domainID, wid, rid, minID, maxID, targets, producer, session)
	}
}

func newKafkaProducer(c *cli.Context) messaging.Producer {
	hostFile := getRequiredOption(c, FlagHostFile)
	destCluster := getRequiredOption(c, FlagCluster)
	destTopic := getRequiredOption(c, FlagTopic)

	// initialize kafka producer
	destBrokers, err := loadBrokers(hostFile, destCluster)
	if err != nil {
		ErrorAndExit("", err)
	}

	config := sarama.NewConfig()
	config.Producer.RequiredAcks = sarama.WaitForAll
	config.Producer.Return.Successes = true
	sproducer, err := sarama.NewSyncProducer(destBrokers, config)
	if err != nil {
		ErrorAndExit("", err)
	}
	logger := bark.NewNopLogger()

	producer := messaging.NewKafkaProducer(destTopic, sproducer, logger)
	return producer
}

// AdminMergeDLQ publish replication tasks from DLQ or JSON file
func AdminMergeDLQ(c *cli.Context) {
	hostFile := getRequiredOption(c, FlagHostFile)
	producer := newKafkaProducer(c)

	var err error
	var inFile string
	var tasks []*replicator.ReplicationTask
	if c.IsSet(FlagInputFile) && (c.IsSet(FlagInputCluster) || c.IsSet(FlagInputTopic) || c.IsSet(FlagStartOffset)) {
		ErrorAndExit("", fmt.Errorf("ONLY Either from JSON file or from DLQ topic"))
	}

	if c.IsSet(FlagInputFile) {
		inFile = c.String(FlagInputFile)
		// parse json input as replicaiton tasks
		tasks, err = parseReplicationTask(inFile)
		if err != nil {
			ErrorAndExit("", err)
		}
		// publish to topic
		for idx, t := range tasks {
			err := producer.Publish(t)
			if err != nil {
				fmt.Printf("cannot publish task %v to topic \n", idx)
				ErrorAndExit("", err)
			} else {
				fmt.Printf("replication task sent: %v firstID %v, nextID %v \n", idx, t.HistoryTaskAttributes.GetFirstEventId(), t.HistoryTaskAttributes.GetNextEventId())
			}
		}
	} else {
		fromTopic := getRequiredOption(c, FlagInputTopic)
		fromCluster := getRequiredOption(c, FlagInputCluster)
		startOffset := c.Int64(FlagStartOffset)
		group := getRequiredOption(c, FlagGroup)

		fromBrokers, err := loadBrokers(hostFile, fromCluster)
		if err != nil {
			ErrorAndExit("", err)
		}

		consumer := createConsumerAndWaitForReady(fromBrokers, group, fromTopic)

		highWaterMarks, ok := consumer.HighWaterMarks()[fromTopic]
		if !ok {
			ErrorAndExit("", fmt.Errorf("cannot find high watermark"))
		}
		fmt.Printf("Topic high watermark %v.\n", highWaterMarks)
		for partition := range highWaterMarks {
			consumer.MarkPartitionOffset(fromTopic, partition, startOffset, "")
			fmt.Printf("reset offset %v:%v \n", partition, startOffset)
		}
		err = consumer.CommitOffsets()
		if err != nil {
			ErrorAndExit("fail to commit offset", err)
		}
		// create consumer again to make sure MarkPartitionOffset works
		consumer = createConsumerAndWaitForReady(fromBrokers, group, fromTopic)

		for {
			select {
			case msg, ok := <-consumer.Messages():
				if !ok {
					return
				}
				if msg.Offset < startOffset {
					fmt.Printf("Wrong Message [%v],[%v] \n", msg.Partition, msg.Offset)
					ErrorAndExit("", fmt.Errorf("offset is not correct"))
					continue
				} else {
					var task replicator.ReplicationTask
					err := decode(msg.Value, &task)
					if err != nil {
						ErrorAndExit("failed to deserialize message due to error", err)
					}

					err = producer.Publish(&task)

					if err != nil {
						fmt.Printf("[Error] Message [%v],[%v] failed: %v\n", msg.Partition, msg.Offset, err)
					} else {
						fmt.Printf("Message [%v],[%v] succeeded\n", msg.Partition, msg.Offset)
					}
				}
				consumer.MarkOffset(msg, "")
			case <-time.After(time.Second * 5):
				fmt.Println("heartbeat: waiting for more messages, Ctrl+C to stop any time...")
			}
		}
	}
}

func createConsumerAndWaitForReady(brokers []string, group, fromTopic string) *cluster.Consumer {
	config := cluster.NewConfig()
	config.Consumer.Return.Errors = true
	config.Consumer.Offsets.Initial = sarama.OffsetOldest
	config.Group.Return.Notifications = true

	client, err := cluster.NewClient(brokers, config)
	if err != nil {
		ErrorAndExit("", err)
	}

	consumer, err := cluster.NewConsumerFromClient(client, group, []string{fromTopic})
	if err != nil {
		ErrorAndExit("", err)
	}

	for ntf := range consumer.Notifications() {
		time.Sleep(time.Second)
		if partitions := ntf.Current[fromTopic]; len(partitions) > 0 && ntf.Type == cluster.RebalanceOK {
			break
		}
		fmt.Println("Waiting for consumer ready...")
	}
	return consumer
}

func parseReplicationTask(in string) (tasks []*replicator.ReplicationTask, err error) {
	file, err := os.Open(in)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	idx := 0
	for scanner.Scan() {
		idx++
		line := strings.TrimSpace(scanner.Text())
		if len(line) == 0 {
			fmt.Printf("line %v is empty, skipped\n", idx)
			continue
		}

		t := &replicator.ReplicationTask{}
		err := json.Unmarshal([]byte(line), t)
		if err != nil {
			fmt.Printf("line %v cannot be deserialized to replicaiton task: %v.\n", idx, line)
			return nil, err
		}
		tasks = append(tasks, t)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return tasks, nil
}

func loadBrokers(hostFile string, cluster string) (brokers []string, err error) {
	contents, err := ioutil.ReadFile(hostFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load kafka cluster info from %v., error: %v", hostFile, err)
	}
	clustersConfig := ClustersConfig{}
	if err := yaml.Unmarshal(contents, &clustersConfig); err != nil {
		return nil, err
	}
	if len(clustersConfig.Clusters) != 0 {
		config, ok := clustersConfig.Clusters[cluster]
		if ok {
			brs := config.Brokers
			for i, b := range brs {
				if !strings.Contains(b, ":") {
					b += ":9092"
					brs[i] = b
				}
			}
			return brs, nil
		}
	}
	return nil, fmt.Errorf("failed to load broker for cluster %v", cluster)
}