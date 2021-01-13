package appmessage

// SubmitBlockRequestMessage is an appmessage corresponding to
// its respective RPC message
type SubmitBlockRequestMessage struct {
	baseMessage
	Block *MsgBlock
}

// Command returns the protocol command string for the message
func (msg *SubmitBlockRequestMessage) Command() MessageCommand {
	return CmdSubmitBlockRequestMessage
}

// NewSubmitBlockRequestMessage returns a instance of the message
func NewSubmitBlockRequestMessage(block *MsgBlock) *SubmitBlockRequestMessage {
	return &SubmitBlockRequestMessage{
		Block: block,
	}
}

type RejectReason byte

const (
	RejectReasonNone RejectReason = iota
	RejectReasonBlockInvalid
	RejectReasonIsInIBD
)

var rejectReasonToString = map[RejectReason]string{
	RejectReasonNone:         "None",
	RejectReasonBlockInvalid: "Block is invalid",
	RejectReasonIsInIBD:      "Node is in IBD",
}

func (rr RejectReason) String() string {
	return rejectReasonToString[rr]
}

// SubmitBlockResponseMessage is an appmessage corresponding to
// its respective RPC message
type SubmitBlockResponseMessage struct {
	baseMessage
	RejectReason RejectReason
	Error        *RPCError
}

// Command returns the protocol command string for the message
func (msg *SubmitBlockResponseMessage) Command() MessageCommand {
	return CmdSubmitBlockResponseMessage
}

// NewSubmitBlockResponseMessage returns a instance of the message
func NewSubmitBlockResponseMessage() *SubmitBlockResponseMessage {
	return &SubmitBlockResponseMessage{}
}
