package main

import (
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/service/s3"
	"hash/fnv"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
)

type s3op struct {
	Event  string `json:"op"`
	Size   uint64 `json:"size"`
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

type workerChan struct {
	workChan chan s3op
	wg       *sync.WaitGroup
}

var hasher = fnv.New64a()
var operations = map[string]bool{"put": true, "get": true, "head": true, "updatemeta": true, "delete": true}

type workloadParams struct {
	// keeps track of keys that have already been hashed to a specific worker
	hashKeys map[string]uint64
	// keeps track if a bucket has already been created for replay
	bucketMap map[string]bool
	// keeps track of each worker channel.Once the
	// hash value for a key is determined it is sent to a worker in this slice
	workersChanSlice []*workerChan
	concurrency      int
	credentials      *credentials.Credentials
}

func setupWorkloadParams(workerChans []*workerChan, concurrency int, credential *credentials.Credentials) *workloadParams {
	keys := make(map[string]uint64)
	buckets := make(map[string]bool)
	return &workloadParams{hashKeys: keys, bucketMap: buckets, workersChanSlice: workerChans, concurrency: concurrency, credentials: credential}
}

func closeAllWorkerChannels(workChanSlice []*workerChan) {
	for i := 0; i < len(workChanSlice); i++ {
		close(workChanSlice[i].workChan)
	}
}

func openFile(filepath string) (*json.Decoder, error) {
	var f *os.File
	var err error
	if f, err = os.Open(filepath); err != nil {
		return nil, err
	}
	return json.NewDecoder(f), nil
}

// Decodes the replay json file generated by the audit-analysis component into
// s3ops slices. Decoding using json arrays allows us to stream the file instead
// of reading the entire file at once. We then send these slices to a channel
// to breakup into individual S3operations to be executed by a worker.
func parseFileReplay(args *parameters, opsChan chan []s3op) {
	if _, err := args.jsonDecoder.Token(); err != nil {
		log.Fatal(err)
	}
	for args.jsonDecoder.More() {
		var ops []s3op
		if err := args.jsonDecoder.Decode(&ops); err != nil {
			log.Fatal(err)
		}
		opsChan <- ops
	}
	return
}

// Parses file and checks to see if worload type is one of {mixed,replay}
// If Replay -> continue streaming in the json file to determine which s3 operations to execute
// If Mixed -> Read in json file to a struct to determine which s3 operations to generate and then execute
func SetupOps(args *parameters, workerChans []*workerChan, credential *credentials.Credentials) error {
	workloadParams := setupWorkloadParams(workerChans, args.concurrency, credential)

	if _, err := args.jsonDecoder.Token(); err != nil {
		return err
	}

	workType, err := args.jsonDecoder.Token()

	if err != nil {
		return err
	}
	switch workType {
	case "mixedWorkload":
		MixedWorkload(args, workloadParams)
	case "replay":
		Replay(args, workloadParams)
	default:
		log.Fatal("Incorrect workload type specified, must be one of 'mixedWorkload' or 'replay'")
	}
	return nil
}

// Starts receiving on the []S3op channel which takes in slice of S3ops and sends
// each s3op to a worker based on hashValue of Name
func Replay(args *parameters, workload *workloadParams) {
	s3opsChan := make(chan []s3op, 1000)
	go func() {
		parseFileReplay(args, s3opsChan)
		// The entire replay file has been read in so
		// close the channel we are receiving the input on
		close(s3opsChan)
	}()

	for ops := range s3opsChan {
		splitS3ops(workload, ops, args.endpoints[0], args.region)
	}
}

// Splits up each []s3op into single s3op and sends to approriate worker
func splitS3ops(params *workloadParams, ops []s3op, endpoint string, region string) {
	for _, op := range ops {
		workerNum := getHashKey(params.hashKeys, op.Bucket+op.Key, params.concurrency)
//		op.Bucket = fmt.Sprintf("%s-%d", op.Bucket, workerNum)
/*
		if _, ok := params.bucketMap[op.Bucket]; !ok {
			err := createBucket(params.bucketMap, op.Bucket, endpoint, region, params.credentials)
			if err != nil {
				log.Fatalf("Unable to create bucket %s for s3 workload %v", op.Bucket, err)
			}
		}
*/
		params.workersChanSlice[workerNum].workChan <- op
	}
}

type opTrack struct {
	ops    float64
	sent   int64
	Optype string `json:"operationType"`
	Ratio  int    `json:"ratio"`
}

// Main mixedWorkload function, creates a struct to track relative ratios
func MixedWorkload(args *parameters, workloadParams *workloadParams) {
	ratios := parseFileMixed(args)
	generateRequests(args, ratios, workloadParams)
}

// Parses mixedReplayFile into a struct
func parseFileMixed(args *parameters) []opTrack {
	ratios := []opTrack{}
	for args.jsonDecoder.More() {
		if err := args.jsonDecoder.Decode(&ratios); err != nil {
			log.Fatal(err)
		}
	}
	totalPerc := 0
	for _, v := range ratios {
		if _, ok := operations[v.Optype]; !ok {
			log.Fatalf("Mixed workload operation types must be one of {'put','get','delete','updatemeta','head'}, but got %v", v.Optype)
		}
		v.ops = ((float64(args.nrequests.value) * float64(v.Ratio)) / float64(100))
		totalPerc += v.Ratio
	}
	if totalPerc != 100 {
		log.Fatal("Pertcange of operations does not sum to 100")
	}
	return ratios
}

// Generates s3ops based on mixedWorkload
// Generates a workload 100 mixed operations at a time. For instance, if 50% Put and 50% Get specified,
// It will generate 50 Puts and send them. Then it will generate 50 Gets and
// send them. It will repeat this until the number of requests specified is reached.
func generateRequests(args *parameters, ratios []opTrack, workload *workloadParams) {
	sent := 0
	totalOps := args.nrequests.value
	for j := 0; j < int(math.Ceil(float64(totalOps)/100.0)); j++ {
		// Send in batches of 100, However if leftover is < 100
		// adjust the operation's ratio accordingly
		leftover := math.Min(100.0, float64(totalOps-sent))
		for _, v := range ratios {
			for i := 0; i < int(math.Floor((float64(v.Ratio)/100.0)*leftover)); i++ {
				op := s3op{Event: v.Optype, Size: uint64(args.osize), Bucket: args.bucketname, Key: args.objectprefix + "-" + strconv.FormatInt(v.sent, 10)}
				sent += 1
				v.sent += 1
				sendS3op(op, workload, args.endpoints[0], args.region)
			}
		}
	}
}

// Sends s3op to appropriate worker for mixedWorkload
func sendS3op(op s3op, params *workloadParams, endpoint string, region string) {
	workerNum := getHashKey(params.hashKeys, op.Key+op.Bucket, params.concurrency)
//	op.Bucket = fmt.Sprintf("%s-%d", op.Bucket, workerNum)
/*
	if _, ok := params.bucketMap[op.Bucket]; !ok {
		err := createBucket(params.bucketMap, op.Bucket, endpoint, region, params.credentials)
		if err != nil {
			log.Fatalf("Unable to create bucket %s for s3 workload %v", op.Bucket, err)
		}
	}
*/
	params.workersChanSlice[workerNum].workChan <- op
}

// creates a new Bucket
func createBucket(currentBuckets map[string]bool, bucket string, endpoint string, region string, credential *credentials.Credentials) error {
	httpClient := MakeHTTPClient()

	svc := MakeS3Service(httpClient, int(0), int(0), endpoint, region, "", credential)

	// check for bucket existence
	_, err := svc.CreateBucket(&s3.CreateBucketInput{Bucket: aws.String(bucket),
		CreateBucketConfiguration: &s3.CreateBucketConfiguration{
			LocationConstraint: aws.String(region),
		},
	})

	if err != nil {
		return err
	}
	if err = svc.WaitUntilBucketExists(&s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		return err
	}

	currentBuckets[bucket] = true
	return nil
}

// allocates an individual channel for each worker
func createChannels(concurrency int, workerWG *sync.WaitGroup) []*workerChan {
	workerChanMap := make([]*workerChan, concurrency)
	for i := 0; i < concurrency; i++ {
		s3opChan := make(chan s3op, 100)
		workerChanMap[i] = &workerChan{workChan: s3opChan, wg: workerWG}
	}
	return workerChanMap
}

// Returns the value for a key if it has already been hashed; otherwise generates one
func getHashKey(hashKeyNames map[string]uint64, keyname string, concurrency int) uint64 {
	if v, ok := hashKeyNames[keyname]; ok {
		return v
	}
	return generateHashKey(hashKeyNames, keyname, concurrency)
}

// generates the hashKey for the worker
func generateHashKey(hashKeyNames map[string]uint64, keyname string, concurrency int) uint64 {
	hasher.Write([]byte(keyname))
	n := hasher.Sum64() % uint64(concurrency)
	if len(hashKeyNames) >= 100000 {
		for k, _ := range hashKeyNames {
			delete(hashKeyNames, k)
			break
		}
	}
	// reset so next write for hashing is always on a clean buffer
	hasher.Reset()
	hashKeyNames[keyname] = n
	return n
}

// mocks up metadata for metadata update
func metadataValue(size int) string {
	size = size / 2
	return strings.Repeat("k", size) + "=" + strings.Repeat("v", size)
}
