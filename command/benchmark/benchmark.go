package benchmark

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/0xPolygon/polygon-edge/benchmark"
	"github.com/0xPolygon/polygon-edge/command"
	"github.com/spf13/cobra"
)

const (
	rootJSONRPCFlag  = "rootJSONRPC"
	childJSONRPCFlag = "childJSONRPC"
	privateKeyFlag   = "privateKey"
	startClusterFlag = "startCluster"
)

type benchmarkParams struct {
	rootJSONRPC  string
	childJSONRPC string
	privateKey   string
	startCluster bool
}

type benchmarkResult struct {
	name   string
	result string
}

var (
	params *benchmarkParams = &benchmarkParams{}
)

func GetCommand() *cobra.Command {
	benchmarkCmd := &cobra.Command{
		Use:     "benchmark-test",
		Short:   "Run benchmark tests",
		Example: getCmdExample(),
		PreRunE: preRunCommand,
		Run:     runCommand,
	}

	setFlags(benchmarkCmd)

	return benchmarkCmd
}

func runCommand(cmd *cobra.Command, _ []string) {
	outputter := command.InitializeOutputter(cmd)
	defer outputter.WriteOutput()

	// set up the testing environment
	testing.Init()

	// set testing params
	benchmark.RootJSONRPC = &params.rootJSONRPC
	benchmark.ChildJSONRPC = &params.childJSONRPC
	benchmark.PrivateKey = &params.privateKey
	benchmark.StartCluster = &params.startCluster

	// set up environment, get test cases and clean up fn
	testCases, cleanUpFn := benchmark.RootChildSendTxSetUp(&testing.B{})
	defer cleanUpFn()

	// Loop over the test cases and call the benchmark test
	for _, testInput := range testCases {
		sendTxResult := testing.Benchmark(func(b *testing.B) {
			b.Helper()
			benchmark.TxTestCasesExecutor(b, testInput)
		})
		benchmarkResult := &benchmarkResult{
			name:   testInput.Name,
			result: fmt.Sprintf("%s %s", sendTxResult.String(), sendTxResult.MemString()),
		}
		outputter.SetCommandResult(benchmarkResult)
		outputter.WriteOutput()
	}
}

func setFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(
		&params.rootJSONRPC,
		rootJSONRPCFlag,
		"",
		"JSONRPC address of the root node",
	)
	cmd.Flags().StringVar(
		&params.childJSONRPC,
		childJSONRPCFlag,
		"",
		"JSONRPC address of the child node",
	)
	cmd.Flags().StringVar(
		&params.privateKey,
		privateKeyFlag,
		"",
		"private key that will be used to send tx (it is expected that this address has enough funds)",
	)
	cmd.Flags().BoolVar(
		&params.startCluster,
		startClusterFlag,
		false,
		"startCluster tells if the local cluster should be started",
	)

	cmd.MarkFlagsMutuallyExclusive(startClusterFlag, rootJSONRPCFlag)
	cmd.MarkFlagsMutuallyExclusive(startClusterFlag, childJSONRPCFlag)
}

func (br *benchmarkResult) GetOutput() string {
	var buffer bytes.Buffer

	buffer.WriteString(fmt.Sprintf("\n%s: %s\n", br.result, br.name))

	return buffer.String()
}

func getCmdExample() string {
	return fmt.Sprintf("%s %s %s %s %s", "polygon-edge", "benchmark-test",
		"--childJSONRPC=\"http://127.0.0.1:12001\"", "--rootJSONRPC=\"http://127.0.0.1:8545\"",
		"--privateKey=\"aa75e9a7d427efc732f8e4f1a5b7646adcc61fd5bae40f80d13c8419c9f43d6d\"")
}

func preRunCommand(_ *cobra.Command, _ []string) error {
	if !params.startCluster {
		if params.rootJSONRPC == "" || params.childJSONRPC == "" || params.privateKey == "" {
			return errors.New("if startCluster is not set then rootJSONRPC, childJSONRPC and privateKey must be set")
		}
	}

	return nil
}
