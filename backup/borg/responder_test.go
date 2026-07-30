package borg

import "testing"

// An empty response reaches the parsers whenever borg exits without writing
// anything — an OOM kill or a signal, which Container.Exec reports as
// (exitCode, "", nil). Both parsers index item[0:1] on the first split element,
// which panics on the single "" element strings.Split returns for "".
//
// The contract is a failure, not silence. No output from borg is always a fault on
// this host: a healthy repository prints a payload and a missing one exits 2 with an
// ERROR record. A nil *LogMessage would route the zero RepositoryResponse into
// FindRepository's Repository.DoesNotExist branch and turn that fault into an
// auto-init attempt. The MsgID stays empty so a reason we synthesized can never be
// taken for borg's verdict, and cannot reach the auto-init branch.
func TestReadRepoResponseEmpty(t *testing.T) {
	repoResponse, logMsg := readRepoResponse("")
	if repoResponse != (RepositoryResponse{}) {
		t.Errorf("Received %+v, wanted the zero RepositoryResponse", repoResponse)
	}
	if logMsg == nil {
		t.Fatal("Received nil, wanted a failure")
	}
	if logMsg.MsgID != "" {
		t.Errorf("Received msgid %q, wanted no msgid", logMsg.MsgID)
	}
	if logMsg.Message == "" {
		t.Error("Received an empty message, wanted the fault described")
	}
}

// Same contract for the contents parser, with a second reason: reporting no failure
// would let Repository.Sync upsert zero sizes and an empty archive list, blanking the
// controller's archive view for this volume until the next successful sync.
func TestReadRepoContentResponseEmpty(t *testing.T) {
	repoResponse, logMsg := readRepoContentResponse("")
	if len(repoResponse.Archives) != 0 {
		t.Errorf("Received %d archives, wanted none", len(repoResponse.Archives))
	}
	if repoResponse.Encryption != (EncryptionItem{}) || repoResponse.Repository != (RepositoryItem{}) {
		t.Errorf("Received %+v, wanted the zero RepositoryContentResponse", repoResponse)
	}
	if logMsg == nil {
		t.Fatal("Received nil, wanted a failure")
	}
	if logMsg.MsgID != "" {
		t.Errorf("Received msgid %q, wanted no msgid", logMsg.MsgID)
	}
	if logMsg.Message == "" {
		t.Error("Received an empty message, wanted the fault described")
	}
}
