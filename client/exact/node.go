package exact

import (
	"context"
	"errors"
	"fmt"

	"github.com/gnolang/gno/gno.land/pkg/gnoclient"
	"github.com/gnolang/gno/gno.land/pkg/gnoland"
	"github.com/gnolang/gno/tm2/pkg/amino"
	rpcclient "github.com/gnolang/gno/tm2/pkg/bft/rpc/client"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/std"
)

// gasPricePath is the ABCI query for the gas price of the chain's last block.
const gasPricePath = "auth/gasprice"

// NewGnoclientNode adapts a gnoclient.Client to the chain access a payment
// needs. The client needs no signer: the scheme's Signer signs, and nothing is
// broadcast through here. It holds no mutable state beyond the client handle,
// so one instance is safe to share across concurrent payments.
//
// The client's RPC timeout bounds every read, 60 seconds unless the RPC client
// was made with rpcclient.WithRequestTimeout, and so does the context each
// read is given. Nothing is signed until the reads have answered, so a read
// abandoned costs nothing.
func NewGnoclientNode(cli *gnoclient.Client) *GnoclientNode {
	return &GnoclientNode{cli: cli}
}

// GnoclientNode is the gnoclient-backed chain access returned by
// NewGnoclientNode.
//
// It reads through the client's RPC client rather than gnoclient's own account
// queries, which take no context and drop the chain's answer to a query it
// refused: a payment's reads have to stop when its caller gives up, and a
// session the chain does not hold has to read as that, not as an address with
// no account to fund. The decoding is gnoclient's: the same paths, decoded with
// amino into gno.land's own account types.
type GnoclientNode struct {
	cli *gnoclient.Client
}

var _ Node = (*GnoclientNode)(nil)

// rpc is the client's RPC client, or gnoclient.ErrMissingRPCClient when there
// is none to read through, a nil one inside the interface included.
func (n *GnoclientNode) rpc() (rpcclient.Client, error) {
	if n == nil || n.cli == nil || isNil(n.cli.RPCClient) {
		return nil, gnoclient.ErrMissingRPCClient
	}
	return n.cli.RPCClient, nil
}

// NodeChainID is the chain id the node reports for itself.
func (n *GnoclientNode) NodeChainID(ctx context.Context) (string, error) {
	rpc, err := n.rpc()
	if err != nil {
		return "", err
	}
	status, err := rpc.Status(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("query node status: %w", err)
	}
	if status == nil {
		return "", errors.New("query node status: the node returned no result")
	}
	return status.NodeInfo.Network, nil
}

// query runs the ABCI query path and returns the data it answers with. A query
// the chain refused returns the chain's own abci.Error, wrapped, so a caller
// can tell what the chain decided by its type.
func (n *GnoclientNode) query(ctx context.Context, path string) ([]byte, error) {
	rpc, err := n.rpc()
	if err != nil {
		return nil, err
	}
	res, err := rpc.ABCIQuery(ctx, path, []byte{})
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", path, err)
	}
	if res == nil {
		return nil, fmt.Errorf("query %s: the node returned no result", path)
	}
	if res.Response.Error != nil {
		return nil, fmt.Errorf("query %s: %s: %w", path, res.Response.Log, res.Response.Error)
	}
	return res.Response.Data, nil
}

// Account reads addr's account. An address the chain holds no account for is
// std.ErrUnknownAddress, as gnoclient reports it.
func (n *GnoclientNode) Account(ctx context.Context, addr crypto.Address) (std.Account, error) {
	data, err := n.query(ctx, "auth/accounts/"+addr.String())
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || string(data) == "null" {
		return nil, std.ErrUnknownAddress("unknown address: " + addr.String())
	}
	var acc gnoland.GnoAccount
	if err := amino.UnmarshalJSON(data, &acc); err != nil {
		return nil, fmt.Errorf("decode the account of %s: %w", addr, err)
	}
	return &acc.BaseAccount, nil
}

// SessionAccount reads the session account session holds under master. A
// session the chain does not hold, such as a revoked one, is the chain's
// std.SessionNotFoundError.
func (n *GnoclientNode) SessionAccount(ctx context.Context, master, session crypto.Address) (std.Account, error) {
	data, err := n.query(ctx, "auth/accounts/"+master.String()+"/session/"+session.String())
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || string(data) == "null" {
		return nil, std.ErrSessionNotFound("no session " + session.String() + " under " + master.String())
	}
	var acc gnoland.GnoSessionAccount
	if err := amino.UnmarshalJSON(data, &acc); err != nil {
		return nil, fmt.Errorf("decode session %s of %s: %w", session, master, err)
	}
	return &acc, nil
}

// GasPrice reads the gas price of the chain's last block the way gnokey reads
// it to estimate a fee. A chain that has stored no price answers gas 0 at an
// empty price, which is no price, as is an answer with nothing in it.
func (n *GnoclientNode) GasPrice(ctx context.Context) (*std.GasPrice, error) {
	data, err := n.query(ctx, gasPricePath)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	var price std.GasPrice
	if err := amino.UnmarshalJSON(data, &price); err != nil {
		return nil, fmt.Errorf("decode gas price %q: %w", data, err)
	}
	if price.Gas == 0 || price.Price.Amount == 0 {
		return nil, nil
	}
	return &price, nil
}
