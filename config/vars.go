package config

import (
	"os"

	"github.com/AnomalyFi/anchor/common"
)

var (
	// Version is set at build time (must be a var, not a const!)
	Version = "v1.7-dev"

	// RFC3339Milli is a time format string based on time.RFC3339 but with millisecond precision
	RFC3339Milli = "2006-01-02T15:04:05.999Z07:00"

	// ServerReadTimeoutMs sets the maximum duration for reading the entire request, including the body. A zero or negative value means there will be no timeout.
	ServerReadTimeoutMs = common.GetEnvInt("MEV_BOOST_SERVER_READ_TIMEOUT_MS", 1000)

	// ServerReadHeaderTimeoutMs sets the amount of time allowed to read request headers.
	ServerReadHeaderTimeoutMs = common.GetEnvInt("MEV_BOOST_SERVER_READ_HEADER_TIMEOUT_MS", 1000)

	// ServerWriteTimeoutMs sets the maximum duration before timing out writes of the response.
	ServerWriteTimeoutMs = common.GetEnvInt("MEV_BOOST_SERVER_WRITE_TIMEOUT_MS", 0)

	// ServerIdleTimeoutMs sets the maximum amount of time to wait for the next request when keep-alives are enabled.
	ServerIdleTimeoutMs = common.GetEnvInt("MEV_BOOST_SERVER_IDLE_TIMEOUT_MS", 0)

	// ServerMaxHeaderBytes defines the max header byte size for requests (for dos prevention)
	ServerMaxHeaderBytes = common.GetEnvInt("MAX_HEADER_BYTES", 4000)

	// SkipRelaySignatureCheck can be used to disable relay signature check
	SkipRelaySignatureCheck = os.Getenv("SKIP_RELAY_SIGNATURE_CHECK") == "1"

	SlotTimeSec = uint64(common.GetEnvInt("SLOT_SEC", common.SlotTimeSecMainnet))

	// Seq related fields
	SeqSigningKey = common.GetEnv("SEQ_KEY", "323b1d8f4eed5f0da9da93071b034f2dce9d2d22692c172f3cb252a64ddfafd01b057de320297c29ad0c1f589ea216869cf1938d88c9fbd70d6748323dbf2fa7") // ed25519 private key hex
	SeqURI        = common.GetEnv("SEQ_URI", "http://127.0.0.1:9652/ext/bc/2GzsD6nCJ5nRrhFAbCF16hYPFJ3VJUUUqyyJfHXckSxkyturkH")
	SeqChainID    = common.GetEnv("SEQ_CHAIN_ID", "2GzsD6nCJ5nRrhFAbCF16hYPFJ3VJUUUqyyJfHXckSxkyturkH")
	SeqNetworkID  = common.GetEnvInt("SEQ_NETWORK_ID", 1337)
)
