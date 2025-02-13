package keeper

import (
	"bytes"
	"fmt"

	"github.com/Stride-Labs/stride/x/interchainquery/types"
	icqtypes "github.com/Stride-Labs/stride/x/interchainquery/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
)

// ___________________________________________________________________________________________________

// Callbacks wrapper struct for interchainstaking keeper
type Callback func(Keeper, sdk.Context, []byte, types.Query) error

type Callbacks struct {
	k         Keeper
	callbacks map[string]Callback
}

var _ types.QueryCallbacks = Callbacks{}

func (k Keeper) CallbackHandler() Callbacks {
	return Callbacks{k, make(map[string]Callback)}
}

//callback handler
func (c Callbacks) Call(ctx sdk.Context, id string, args []byte, query types.Query) error {
	return c.callbacks[id](c.k, ctx, args, query)
}

func (c Callbacks) Has(id string) bool {
	_, found := c.callbacks[id]
	return found
}

func (c Callbacks) AddCallback(id string, fn interface{}) types.QueryCallbacks {
	c.callbacks[id] = fn.(Callback)
	return c
}

func (c Callbacks) RegisterCallbacks() types.QueryCallbacks {
	a := c.AddCallback("withdrawalbalance", Callback(WithdrawalBalanceCallback))
	return a.(Callbacks)
}

// -----------------------------------
// Callback Handlers
// -----------------------------------

// WithdrawalBalanceCallback is a callback handler for WithdrawalBalance queries.
func WithdrawalBalanceCallback(k Keeper, ctx sdk.Context, args []byte, query icqtypes.Query) error {
	// NOTE(TEST-112) for now, to get proofs in your ICQs, you need to query the entire store on the host zone! e.g. "store/bank/key"

	zone, found := k.GetHostZone(ctx, query.GetChainId())
	if !found {
		return fmt.Errorf("no registered zone for chain id: %s", query.GetChainId())
	}
	balancesStore := []byte(query.Request[1:])
	accAddr, err := banktypes.AddressFromBalancesStore(balancesStore)
	if err != nil {
		return err
	}

	//TODO(TEST-112) revisit this code, it's not vetted
	coin := sdk.Coin{}
	err = k.cdc.Unmarshal(args, &coin)
	if err != nil {
		k.Logger(ctx).Error("unable to unmarshal balance info for zone", "zone", zone.ChainId, "err", err)
		return err
	}

	if coin.IsNil() {
		denom := ""

		for i := 0; i < len(query.Request)-len(accAddr); i++ {
			if bytes.Equal(query.Request[i:i+len(accAddr)], accAddr) {
				denom = string(query.Request[i+len(accAddr):])
				break
			}

		}
		// if balance is nil, the response sent back is nil, so we don't receive the denom. Override that now.
		coin = sdk.NewCoin(denom, sdk.ZeroInt())
	}

	// sanity check, do not transfer if we have 0 balance!
	if coin.Amount.Int64() == 0 {
		k.Logger(ctx).Info("WithdrawalBalanceCallback: no balance to transfer", "zone", zone.ChainId, "accAddr", accAddr, "coin", coin)
		return nil
	}

	// Set withdrawal balance as attribute on HostZone's withdrawal ICA account
	wa := zone.GetWithdrawalAccount()
	wa.Balance = coin.Amount.Int64()
	zone.WithdrawalAccount = wa
	k.SetHostZone(ctx, zone)
	k.Logger(ctx).Info(fmt.Sprintf("Just set WithdrawalBalance to: %d", wa.Balance))
	ctx.EventManager().EmitEvents(sdk.Events{
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute("hostZone", zone.ChainId),
			sdk.NewAttribute("totalWithdrawalBalance", coin.Amount.String()),
		),
	})

	// Sweep the withdrawal account balance, to the commission and the delegation accounts
	k.Logger(ctx).Info(fmt.Sprintf("ICA Bank Sending %d%s from withdrawalAddr to delegationAddr.", coin.Amount.Int64(), coin.Denom))

	withdrawalAccount := zone.GetWithdrawalAccount()
	delegationAccount := zone.GetDelegationAccount()
	// TODO(TEST-112) set the stride revenue address in a config file
	REV_ACCT := "cosmos1wdplq6qjh2xruc7qqagma9ya665q6qhcwju3ng"

	params := k.GetParams(ctx)
	strideCommission := sdk.NewDec(int64(params.GetStrideCommission())).Quo(sdk.NewDec(100)) // convert to decimal
	withdrawalBalance := sdk.NewDec(coin.Amount.Int64())
	// TODO(TEST-112) don't perform unsafe uint64 to int64 conversion
	strideClaim := strideCommission.Mul(withdrawalBalance)
	strideClaimFloored := strideClaim.TruncateInt()

	reinvestAmount := (sdk.NewDec(1).Sub(strideCommission)).Mul(withdrawalBalance)
	reinvestAmountCeil := reinvestAmount.Ceil().TruncateInt()

	// TODO(TEST-112) safety check, balances should add to original amount
	if (strideClaimFloored.Int64() + reinvestAmountCeil.Int64()) != coin.Amount.Int64() {
		ctx.Logger().Error(fmt.Sprintf("Error with withdraw logic: %d, Fee portion: %d, reinvestPortion %d", coin.Amount.Int64(), strideClaimFloored.Int64(), reinvestAmountCeil.Int64()))
		return sdkerrors.Wrap(sdkerrors.ErrInvalidRequest, "Failed to subdivide rewards to feeAccount and delegationAccount")
	}
	strideCoin := sdk.NewCoin(coin.Denom, strideClaimFloored)
	reinvestCoin := sdk.NewCoin(coin.Denom, reinvestAmountCeil)

	var msgs []sdk.Msg
	// construct the msg
	msgs = append(msgs, &banktypes.MsgSend{FromAddress: withdrawalAccount.GetAddress(),
		ToAddress: REV_ACCT, Amount: sdk.NewCoins(strideCoin)})
	msgs = append(msgs, &banktypes.MsgSend{FromAddress: withdrawalAccount.GetAddress(),
		ToAddress: delegationAccount.GetAddress(), Amount: sdk.NewCoins(reinvestCoin)})

	ctx.Logger().Info(fmt.Sprintf("Submitting withdrawal sweep messages for: %v", msgs))

	// Send the transaction through SubmitTx
	_, err = k.SubmitTxsStrideEpoch(ctx, zone.ConnectionId, msgs, *withdrawalAccount)
	if err != nil {
		return sdkerrors.Wrapf(sdkerrors.ErrInvalidRequest, "Failed to SubmitTxs for %s, %s, %s", zone.ConnectionId, zone.ChainId, msgs)
	}

	return nil
}
