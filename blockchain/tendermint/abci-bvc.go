package tendermint

import (
	"context"
	//"crypto/ed25519"
	"crypto/sha256"
	"github.com/AccumulateNetwork/SMT/pmt"
	"github.com/tendermint/tendermint/crypto/ed25519"
	tmnet "github.com/tendermint/tendermint/libs/net"
	"github.com/tendermint/tendermint/rpc/client/local"
	core_grpc "github.com/tendermint/tendermint/rpc/grpc"
	"google.golang.org/grpc"
	"net"

	"encoding/json"
	"fmt"
	"github.com/AccumulateNetwork/accumulated/scratch/example/code"
	cryptoenc "github.com/tendermint/tendermint/crypto/encoding"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/spf13/viper"
	cfg "github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/crypto"
	tmflags "github.com/tendermint/tendermint/libs/cli/flags"
	"github.com/tendermint/tendermint/libs/log"
	nm "github.com/tendermint/tendermint/node"

	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/privval"
	"github.com/tendermint/tendermint/proxy"
	rpctypes "github.com/tendermint/tendermint/rpc/jsonrpc/types"
	dbm "github.com/tendermint/tm-db"

	"github.com/tendermint/tendermint/libs/service"
	"github.com/tendermint/tendermint/version"
	"os"

	"bytes"
	"github.com/AccumulateNetwork/SMT/managed"
	vadb "github.com/AccumulateNetwork/ValidatorAccumulator/ValAcc/database"

	valacctypes "github.com/AccumulateNetwork/ValidatorAccumulator/ValAcc/types"
	pb "github.com/AccumulateNetwork/accumulated/api/proto"
	"github.com/AccumulateNetwork/accumulated/blockchain/validator"
	vtypes "github.com/AccumulateNetwork/accumulated/blockchain/validator/types"
	abcitypes "github.com/tendermint/tendermint/abci/types"
	"sync"
	//"time"
	smtdb "github.com/AccumulateNetwork/SMT/storage/database"
)


//
//var (
//	stateKey        = []byte("stateKey")
//	kvPairPrefixKey = []byte("kvPairKey:")
//
//	ProtocolVersion uint64 = 0x1
//)
//
//type State struct {
//	db      dbm.DB
//	Size    int64  `json:"size"`
//	Height  int64  `json:"height"`
//	AppHash []byte `json:"app_hash"`
//}

func loadState(db dbm.DB) State {
	var state State
	state.db = db
	stateBytes, err := db.Get(stateKey)
	if err != nil {
		panic(err)
	}
	if len(stateBytes) == 0 {
		return state
	}
	err = json.Unmarshal(stateBytes, &state)
	if err != nil {
		panic(err)
	}
	return state
}

func saveState(state State) {
	stateBytes, err := json.Marshal(state)
	if err != nil {
		panic(err)
	}
	err = state.db.Set(stateKey, stateBytes)
	if err != nil {
		panic(err)
	}
}

func prefixKey(key []byte) []byte {
	return append(kvPairPrefixKey, key...)
}
type MerkleManagerState struct {
	merklemgr *managed.MerkleManager
	currentstateobject vtypes.StateObject
	stateobjects []vtypes.StateObject //all the state objects for this height, if we don't care about history this can go byebye...
}

type AccumulatorVMApplication struct {

	abcitypes.BaseApplication
	RetainBlocks int64
	mutex sync.Mutex
	waitgroup sync.WaitGroup

	Height int64

	ChainId [32]byte

	tmvalidators map[string]crypto.PubKey
	//Val *validator.ValidatorContext //change to use chainval below instead
	chainval map[uint64]*validator.ValidatorContext //use this instead to make a group of validators that can be accessed via chain address.


	//begin deprecation
	DB vadb.DB

	state State

	valTypeRegDB    dbm.DB
	//end deprecation

	config *cfg.Config
	Address crypto.Address
	Key privval.FilePVKey
	RPCContext rpctypes.Context
	server service.Service
	amLeader bool
    dbvc pb.BVCEntry



	mmdb smtdb.Manager
	mms map[managed.Hash]*MerkleManagerState
	bpt *pmt.Manager

	lasthash managed.Hash

	txct int64

	timer time.Time

    submission chan pb.Submission
	APIClient core_grpc.BroadcastAPIClient
	Accrpcaddr string
	RouterClient pb.ApiServiceClient

	LocalClient *local.Local
}

func NewAccumulatorVMApplication(ConfigFile string, WorkingDir string) *AccumulatorVMApplication {
	name := "kvstore"
	db, err := dbm.NewGoLevelDB(name, WorkingDir)
	if err != nil {
		panic(err)
	}

	state := loadState(db)

	app := AccumulatorVMApplication{
		//router: new(router2.Router),
		RetainBlocks: 1, //only retain current block, we will manage our own state
		chainval: make(map[uint64]*validator.ValidatorContext),
		state : state,
	}
	app.Initialize(ConfigFile, WorkingDir)

    return &app
}


var _ abcitypes.Application = (*AccumulatorVMApplication)(nil)


func (app *AccumulatorVMApplication) AddValidator(val *validator.ValidatorContext) error {
	//validators are mapped to registered type id's.
	//getTypeId(val.GetChainId())

	//so perhaps, the validator should lookup typeid by chainid in the validator registration database.

	//TODO: Revisit chainid to address.
	app.chainval[val.GetTypeId()] = val
	return nil
}

func (app *AccumulatorVMApplication) GetHeight ()(int64) {
	//
	//app.mutex.Lock()
	//ret := uint64(app.Val.GetCurrentHeight())
	//app.mutex.Unlock()
	//
	return app.state.Height
}

func (app *AccumulatorVMApplication) Info(req abcitypes.RequestInfo) abcitypes.ResponseInfo {

	//todo: load up the merkle databases to the same state we're at...
	//smt.Load(app.state.Height)
	//smt.PruneToHeight(app.state.Height)



	return abcitypes.ResponseInfo{
		Data:             fmt.Sprintf("{\"size\":%v}", app.state.Size),
		Version:          version.ABCIVersion,
		AppVersion:       ProtocolVersion,
		LastBlockHeight:  app.state.Height,
		LastBlockAppHash: app.state.AppHash,
	}
}

func (app *AccumulatorVMApplication) SetOption(req abcitypes.RequestSetOption) abcitypes.ResponseSetOption {
	return app.BaseApplication.SetOption(req)
}

func (app *AccumulatorVMApplication) GetLocalClient() (local.Local, error) {
	return *app.LocalClient, nil
}
func (app *AccumulatorVMApplication) GetAPIClient() (core_grpc.BroadcastAPIClient, error) {
	return app.APIClient, nil
}

func (app *AccumulatorVMApplication) Initialize(ConfigFile string, WorkingDir string) error {

	app.waitgroup.Add(1)
	fmt.Printf("Starting Tendermint (version: %v)\n", version.ABCIVersion)

	app.config = cfg.DefaultConfig()
	app.config.SetRoot(WorkingDir)

	v := viper.New()
	v.SetConfigFile(ConfigFile)
	v.AddConfigPath(WorkingDir)
	if err := v.ReadInConfig(); err != nil {

		return fmt.Errorf("viper failed to read config file: %w", err)
	}
	if err := v.Unmarshal(app.config); err != nil {
		return fmt.Errorf("viper failed to unmarshal config: %w", err)
	}
	if err := app.config.ValidateBasic(); err != nil {
		return fmt.Errorf("config is invalid: %w", err)
	}
	app.Accrpcaddr = v.GetString("accumulate.AccRPCAddress")

	//create a connection to the router.
	routeraddress := v.GetString("accumulate.RouterAddress")
	if len(routeraddress) == 0 {
		return fmt.Errorf("accumulate.RouterAddress token not specified in config file")
	}

	conn, err := grpc.Dial(routeraddress, grpc.WithBlock(), grpc.WithInsecure(), grpc.WithContextDialer(dialerFunc))
	if err != nil {
		return fmt.Errorf("Error Openning GRPC client in router")
	}
	//defer conn.Close()
	app.RouterClient = pb.NewApiServiceClient(conn)

	name := "blockstate"
	db, err := dbm.NewGoLevelDB(name, WorkingDir)
	if err != nil {
		panic(err)
	}

	app.state = loadState(db)

	str := "ValTypeReg"
	fmt.Printf("Creating %s\n", str)
	cdb, err := nm.DefaultDBProvider(&nm.DBContext{str, app.config})
	app.valTypeRegDB = cdb
	if err != nil {
		return fmt.Errorf("failed to create node accumulator database: %w", err)
	}

	dbfilename := WorkingDir + "/" + "valacc.db"
	dbtype := "badger"
	//dbtype := "memory" ////for kicks just create an in-memory database for now
	app.mmdb.Init(dbtype,dbfilename)
	app.mms = make(map[managed.Hash]*MerkleManagerState)
	app.bpt = pmt.NewBPTManager(&app.mmdb)

	//the salt is the chainid and needs to be updated as chainid changes with each transaction
	//app.mms = new(map[managed.Hash]*MMState)

	return nil
}

///ABCI call
func (app *AccumulatorVMApplication) InitChain(req abcitypes.RequestInitChain) abcitypes.ResponseInitChain {
	/*
	type RequestInitChain struct {
		Time            time.Time         `protobuf:"bytes,1,opt,name=time,proto3,stdtime" json:"time"`
		ChainId         string            `protobuf:"bytes,2,opt,name=chain_id,json=chainId,proto3" json:"chain_id,omitempty"`
		ConsensusParams *ConsensusParams  `protobuf:"bytes,3,opt,name=consensus_params,json=consensusParams,proto3" json:"consensus_params,omitempty"`
		Validators      []ValidatorUpdate `protobuf:"bytes,4,rep,name=validators,proto3" json:"validators"`
		AppStateBytes   []byte            `protobuf:"bytes,5,opt,name=app_state_bytes,json=appStateBytes,proto3" json:"app_state_bytes,omitempty"`
		InitialHeight   int64             `protobuf:"varint,6,opt,name=initial_height,json=initialHeight,proto3" json:"initial_height,omitempty"`
	}*/

	fmt.Printf("Initalizing Accumulator Router\n")



	app.ChainId = sha256.Sum256([]byte(req.ChainId))



	////an entry bucket --> do do determine if
	//app.mmdb.AddBucket("Entry")
	app.mmdb.AddBucket("Entries-Debug") //items will bet pushed into this bucket as the state entries change
	app.mmdb.AddBucket("StateEntries")
	////commits will be stored here and key'ed via entry hash.
	//app.mmdb.AddBucket("Commit")

	//launch the hash update thread

	//go app.mm.Update()

	for _, v := range req.Validators {
		r := app.updateValidator(v)
		if r.IsErr() {
			//app.logger.Error("Error updating validators", "r", r)
			fmt.Printf("Error updating validators \n")
		}
	}

	app.submission = make (chan pb.Submission)
	//go app.dispatch()

	return abcitypes.ResponseInitChain{AppHash: app.ChainId[:]}
}

///ABCI / block calls
///   BeginBlock <---
///   [CheckTx]
///   [DeliverTx]
///   EndBlock
///   Commit
// ------ BeginBlock -> DeliverTx -> EndBlock -> Commit
// When Tendermint Core has decided on the block, it's transferred to the application in 3 parts:
// BeginBlock, one DeliverTx per transaction and EndBlock in the end.

//Here we create a batch, which will store block's transactions.
func (app *AccumulatorVMApplication) BeginBlock(req abcitypes.RequestBeginBlock) abcitypes.ResponseBeginBlock {
	//app.currentBatch = app.db.NewTransaction(true)
	//app.Height = req.Header.Height
	// reset valset changes
	app.timer = time.Now()



	fmt.Printf("Begin Block %d on shard %s\n", req.Header.Height, req.Header.ChainID)
	app.txct = 0

	/*
		app.ValUpdates = make([]types.ValidatorUpdate, 0)

		// Punish validators who committed equivocation.
		for _, ev := range req.ByzantineValidators {
			if ev.Type == types.EvidenceType_DUPLICATE_VOTE {
				addr := string(ev.Validator.Address)
				if pubKey, ok := app.valAddrToPubKeyMap[addr]; ok {
					app.updateValidator(types.ValidatorUpdate{
						PubKey: pubKey,
						Power:  ev.Validator.Power - 1,
					})
					app.logger.Info("Decreased val power by 1 because of the equivocation",
						"val", addr)
				} else {
					app.logger.Error("Wanted to punish val, but can't find it",
						"val", addr)
				}
			}
		}

	*/
	//TODO: Purge any expired entry / chain commits


	//Identify the leader for this block.
	//if we are the proposer... then we are the leader.
	app.amLeader = bytes.Compare( app.Address.Bytes(), req.Header.GetProposerAddress() ) == 0


	//fmt.Printf("Public Address: 0x%X\n",app.Address)
	//fmt.Printf("Public Address: 0x%X\n",req.Header.GetProposerAddress())

	if app.amLeader {
        //TODO: determine if anything needs to be done here.
	}

	//app.lasthash = managed.Hash{}

    //app.mm.


	//todo: look at changing this to be queried rather than passed to all validators, because they may not need it
	//chainid := req.GetHeader().ChainID
	//for _, v := range app.chainval {
		//v.SetCurrentBlock(req.Header.Height, &req.Header.Time, &chainid)
		//fmt.Printf("Setting current block info for validator %d",k)
	//}
	//app.Val.SetCurrentBlock(req.Header.Height,&req.Header.Time,&chainid)
	return abcitypes.ResponseBeginBlock{}
}

///ABCI / block calls
///   BeginBlock
///   [CheckTx] <---
///   [DeliverTx]
///   EndBlock
///   Commit

// new transaction is added to the Tendermint Core. Check if it is valid.
func (app *AccumulatorVMApplication) CheckTx(req abcitypes.RequestCheckTx) abcitypes.ResponseCheckTx {
	/*
		type RequestCheckTx struct {
			Tx   []byte      `protobuf:"bytes,1,opt,name=tx,proto3" json:"tx,omitempty"`
			Type CheckTxType `protobuf:"varint,2,opt,name=type,proto3,enum=tendermint.abci.CheckTxType" json:"type,omitempty"`
		}*/

	//create a default response
	ret := abcitypes.ResponseCheckTx{Code: 0, GasWanted: 1}

	//the submission is the format of the Tx input
	sub := &pb.Submission{}

	//unpack the request
	err := proto.Unmarshal(req.Tx,sub)

	//check to see if there was an error decoding the submission
	if err != nil {
		//reject it
		return abcitypes.ResponseCheckTx{Code: code.CodeTypeEncodingError, GasWanted: 0,
			Log: fmt.Sprintf("Unable to decode transaction") }
	}

	//get ready to lookup the validator that we need to use for this request
	var val *validator.ValidatorContext

	//resolve the validator's bve to obtain public key for given height
	var key managed.Hash

	//make sure we have a chain id
	if sub.Chainid == nil {
		return abcitypes.ResponseCheckTx{Code: code.CodeTypeEncodingError, GasWanted: 0,
			Log: fmt.Sprintf("Chain ID is not set for transaction %X", sub.Identitychain) }
	}

	//todo: look up validator rules for this chain to make sure we can do what we want here.

	key.Extract(sub.GetChainid())

	//resolve the validator type to use based on the type of the transaction
    if v, ok := app.chainval[uint64(sub.GetInstruction())]; ok {
    	//if not ok, then we probably need to assign a generic default entry validator?
        val = v
	} else {
		return abcitypes.ResponseCheckTx{Code: code.CodeTypeUnauthorized, GasWanted: 0,
			Log: fmt.Sprintf("Validator not found for chain address %X", sub.GetType() ) }
	}


	//do a quick check to make sure this this transaction has a high probability of passing given further testing upon delivery
	err = val.Check(nil, sub.Identitychain, sub.Chainid, sub.Param1, sub.Param2, sub.Data)
	if err != nil {
		ret.Code = 2
		ret.GasWanted = 0
		ret.GasUsed = 0
		ret.Info = fmt.Sprintf("Entry check failed %v on validator %s \n",sub.Type, val.GetInfo().GetNamespace())
		return ret
	}


	//the validator can probably dictate how the transaction is handled, so maybe simple of mapping base validators
	//is where things can start.
	/*****

	//check the type of transaction
	switch sub.GetInstruction() {
	case pb.AccInstruction_Token_Transaction:


	//case pb.Submission_Entry_Reveal:
			//need to check to see if a segwit for the data exists
			//compute entry hash
			//ask validator to do a quick check on command.
			err := val.Check(nil,nil, sub.Chainid, sub.Param1, sub.Param2, sub.Data)
			if err != nil {
				ret.Code = 2
				ret.GasWanted = 0
				ret.GasUsed = 0
				ret.Info = fmt.Sprintf("Entry check failed %v on validator %s \n",sub.Type, val.GetInfo().GetNamespace())
				return ret
			}
	case pb.AccInstruction_Identity_Creation:
		err := val.Check(nil, nil, sub.Chainid, sub.Param1, sub.Param2, sub.Data)
		if err != nil {
			ret.Code = 2
			ret.GasWanted = 0
			ret.GasUsed = 0
			ret.Info = fmt.Sprintf("Entry check failed %v on validator %s \n",sub.Type, val.GetInfo().GetNamespace())
			return ret
		}
		//verify chain commit signature checks out
		//verify EC has a balance
	case pb.AccInstruction_Data_Chain_Creation:

	//case pb.Submission_Data_Entry:
		//val.
	//case pb.Submission_SyntheticTransaction:
	case pb.AccInstruction_Data_Entry:
			//do nothing for now, is this even needed?
	default:
			ret.Code = 1
			ret.Info = fmt.Sprintf("Unknown message type %v on address %v \n",sub.Identitychain, sub.Chainid)
			return ret
	}
	if err != nil {
		ret.Code = 2
		ret.GasWanted = 0
		return ret
	}
		*****/


	//if we get here, the TX, passed reasonable check, so allow for dispatching to everyone else
	return ret
}

func (app *AccumulatorVMApplication) getCurrentState(chainid []byte) (*vtypes.StateObject, error) {
	var ret *vtypes.StateObject
	var key managed.Hash
	key.Extract(chainid)
	if mms := app.mms[key]; mms != nil {
		ret = &mms.currentstateobject
	} else {
		//pull current state from the database.
		data := app.mmdb.Get("StateEntries", "", chainid)
		if data != nil {
			ret = &vtypes.StateObject{}
			err := ret.Unmarshal(data)
			if err != nil {
				return nil, fmt.Errorf("No Current State is Defined")
			}

		}
	}
	return ret,nil
}

func (app *AccumulatorVMApplication) addStateEntry(chainid []byte, entry []byte) error {
	var mms *MerkleManagerState

	hash := sha256.Sum256(entry)
	key := managed.Hash{}
	key.Extract(chainid)
	if mms = app.mms[key]; mms == nil {
		mms = new(MerkleManagerState)
		mms.merklemgr = managed.NewMerkleManager(&app.mmdb,chainid,8)
		app.mms[key] = mms
	}
	data := app.mmdb.Get("StateEntries", "", chainid)
	if data != nil {
		currso := vtypes.StateObject{}
		mms.currentstateobject.PrevStateHash = currso.PrevStateHash
	}
	mms.merklemgr.AddHash(hash)
	mdroot := mms.merklemgr.MainChain.MS.GetMDRoot()

	//The Entry feeds the Entry Hash, and the Entry Hash feeds the State Hash
	//The MD Root is the current state
	mms.currentstateobject.StateHash = mdroot.Bytes()
	//The Entry hash is the hash of the state object being stored
	mms.currentstateobject.EntryHash = hash[:]
	//The Entry is the State object derived from the transaction
	mms.currentstateobject.Entry = entry

	//list of the state objects from the beginning of the block to the end, so don't know if this needs to be kept
	mms.stateobjects = append(mms.stateobjects, mms.currentstateobject )
	return nil
}

func (app *AccumulatorVMApplication) writeStates() []byte {
	//loop through everything and write out states to the database.
	for chainid,v := range app.mms {
		mdroot := v.merklemgr.MainChain.MS.GetMDRoot()
		if mdroot == nil {
			//shouldn't get here, but will reject if I do
			fmt.Printf("Shouldn't get here on writeState() on chain id %X obtaining merkle state", chainid)
			continue
		}

		app.bpt.Bpt.Insert(chainid,*mdroot)
		datatostore,err := v.currentstateobject.Marshal()
		if err != nil {
			//need to log failure
			continue
		}
		app.mmdb.Put("StateEntries","",chainid.Bytes(),datatostore)
		for i := range v.stateobjects {
			data,err := v.stateobjects[i].Marshal()
			if err != nil {
				//shouldn't get here, but will reject if I do
				fmt.Printf("Shouldn't get here on writeState() on chain id %X for updated states", chainid)
				continue
			}

			app.mmdb.Put("StateEntries","",chainid.Bytes(),datatostore)
			///TBD : this is not needed since we are maintaining only current state and not all states
			//just keeping for debug history.
			app.mmdb.Put("Entries-Debug", "", v.stateobjects[i].StateHash, data)
		}
		//delete it...
		delete(app.mms, chainid)
	}
	app.bpt.Bpt.Update()

	return app.bpt.Bpt.Root.Hash[:]
}

//Figure out what to do with the processed validated transaction.  This may include firing off a synthetic TX or simply
//updating the state of the transaction
func (app *AccumulatorVMApplication) processValidatedSubmissionRequest(vdata *validator.ResponseValidateTX) error {
	for i := range vdata.Submissions {

		hash := managed.Hash(sha256.Sum256(vdata.Submissions[i].Data))

		switch vdata.Submissions[i].Instruction {
		case pb.AccInstruction_Scratch_Entry:
			//generate a key for the chain entry
			//store to scratch DB.

			app.addStateEntry(vdata.Submissions[i].Chainid, vdata.Submissions[i].Data )

		case pb.AccInstruction_Data_Entry:

            //if we get to this point we can move scratch chain to this chain perhaps and remove scratch chain?
            //remove from scratch DB
			app.addStateEntry(vdata.Submissions[i].Chainid, vdata.Submissions[i].Data )
		default:
			//generate a synthetic tx and pass to the next round. keep doing that until validators in subsiquent rounds
			//reduce Submissions to Data Entries on their appropriate chains
			//

            //txid stack
            chash := valacctypes.Hash(hash)
			commit, _ /*txid*/ := vtypes.GenerateCommit(vdata.Submissions[i].Data,&chash,false)


			//need to track txid to make sure they get processed....
			if app.amLeader {
				//we may want to reconsider making this a go call since using grpc could delay things considerably.
				//we only need to make sure it is processed by the next EndBlock so place in pending queue.
				var sk valacctypes.PrivateKey
				copy(sk[:],app.Key.PrivKey.Bytes())

                err := vtypes.SignCommit(sk,commit)

                //now we need to make a new submission that has the segwit commit block added.
                //revisit this...  probably need to
                //store the offset to the segwit
                vdata.Submissions[i].Param1 = uint64(len(vdata.Submissions[i].Data)) //signed
				vdata.Submissions[i].Data = append(vdata.Submissions[i].Data, commit...)

                if err != nil {
                	return fmt.Errorf("Error signing validated submission request")
				}

				//var c jsonrpc2.Client

				//var result int
				//err = c.Request(nil, "http://localhost:26611", "broadcast_tx_async", vdata.Submissions[i], &result)
				//msg, _ := proto.Marshal(&vdata.Submissions[i])

				app.RouterClient.ProcessTx(context.Background(),&vdata.Submissions[i])
			}

		}
	}
	return nil
}

///ABCI / block calls
///   BeginBlock
///   [CheckTx]
///   [DeliverTx] <---
///   EndBlock
///   Commit

// Invalid transactions, we again return the non-zero code.
// Otherwise, we add it to the current batch.
func (app *AccumulatorVMApplication) DeliverTx(req abcitypes.RequestDeliverTx) ( response abcitypes.ResponseDeliverTx) {

	ret := abcitypes.ResponseDeliverTx{GasWanted: 1, GasUsed: 0, Data: nil, Code: code.CodeTypeUnknownError}

	sub := &pb.Submission{}

	//unpack the request
	err := proto.Unmarshal(req.Tx,sub)

	if err != nil {
		//reject it
		return abcitypes.ResponseDeliverTx{Code: code.CodeTypeEncodingError, GasWanted: 0,
			Log: fmt.Sprintf("Unable to decode transaction") }
	}

	chainstate, err := app.getCurrentState(sub.GetChainid())

	//not finding the chain id is probably not a big deal if the chain doesn't exist yet, so need better scrutiny of TX
	//if err != nil {
	//	return abcitypes.ResponseDeliverTx{Code: code.CodeTypeUnauthorized, GasWanted: 0,
	//		Log: fmt.Sprintf("Unable to find chain id") }
	//}

	//not findinng the identity may not be a big deal if we are attempting to create one so need more scrutiny of TX
	identitystate, err := app.getCurrentState(sub.GetIdentitychain()) //need the identity chain

	//lack of chain or identity isn't necessarily an error.
	//if err != nil {
	//	ret.Code = code.CodeTypeUnauthorized
	//	ret.Info = fmt.Sprintf("Invalid State Object for Identity %X", sub.GetIdentitychain())
	//}

	currentstate, err := validator.NewStateEntry(identitystate,chainstate, &app.mmdb)


	if err != nil {
		ret.Code = code.CodeTypeEncodingError
		ret.Info = fmt.Sprintf("Unambe to rerieve State Entry for %X", sub.GetChainid())
	}

	//resolve the validator's bve to obtain public key for given height
	if val, ok := app.chainval[uint64(sub.GetInstruction())]; ok {
		//check the type of transaction
		//in reality we will check the type of chain to determine how to handle validation for that chain.


		//pk := ed25519.PublicKey{}
		//copy(pk,sub.Key)
		//ed25519.Verify(idstate.Publickey,data,signature)
		//this will need to be made more robust...
		//privKey := ed25519.GenPrivKey()
		//ppk := ed25519.PubKey{}

		if ed25519.PubKey(sub.Key).VerifySignature(sub.Data, sub.Signature) == false {
			ret.Code = code.CodeTypeEncodingError
			ret.Info = fmt.Sprintf("Unable to verify data for %X, bad signature", sub.GetChainid())
			return ret
		}

		vdata, err := val.Validate(currentstate, sub.Identitychain, sub.Chainid, sub.Param1, sub.Param2, sub.Data)


		//privKey := ed25519.GenPrivKey()
		////gratuitously sign data to add simulted overhead
		//privKey.Sign(sub.Data)


		if err != nil {
			ret.Code = 2
			ret.GasWanted = 0
			ret.GasUsed = 0
			ret.Info = fmt.Sprintf("Entry check failed %v on validator %v \n",sub.Type, val.GetNamespace())
			return ret
		}
		if vdata == nil {
			ret.Code = 2
			ret.GasWanted = 0
			ret.GasUsed = 0
			ret.Info = fmt.Sprintf("Insufficent Entry Data on validator %v \n", val.GetNamespace())
			return ret
		}

		app.processValidatedSubmissionRequest(vdata)

		//for i := range vdata.StateData {
		if vdata.StateData != nil {
			app.addStateEntry(sub.Chainid, vdata.StateData)
		}

		//now we need to store the data returned by the validator and feed into accumulator
		app.txct++
		//fmt.Printf("txct %d\n",app.txct)
/*
		switch sub.Instruction {
		case pb.AccInstruction_Token_URL_Creation,
			pb.AccInstruction_Token_Transaction,
			pb.AccInstruction_Data_Chain_Creation,
			pb.AccInstruction_Data_Entry,
			pb.AccInstruction_Scratch_Chain_Creation,
			pb.AccInstruction_Scratch_Entry,
			pb.AccInstruction_Token_Issue:


			vdata, err := val.Validate(currentstate, sub.Identitychain, sub.Chainid, sub.Param1, sub.Param2, sub.Data)

			if err != nil {
				ret.Code = 2
				ret.GasWanted = 0
				ret.GasUsed = 0
				ret.Info = fmt.Sprintf("Entry check failed %v on validator %v \n",sub.Type, val.GetNamespace())
				return ret
			}
			if vdata == nil {
				ret.Code = 2
				ret.GasWanted = 0
				ret.GasUsed = 0
				ret.Info = fmt.Sprintf("Insufficent Entry Data on validator %v \n", val.GetNamespace())
				return ret
			}
			// if we have vdata, then we need to figure out what to do with it.



		case pb.AccInstruction_Key_Update:
			//do nothing for now
		case pb.AccInstruction_Identity_Creation:
			//do nothing fo rnow

		case pb.AccInstruction_Data_Store:
			//generate Entry Key from Chainid.
			//need to validate the sucker...
			//validate(sub.GetData)
			state := vtypes.StateObject{}
			//ddiiname := sha256.Sum256([]byte("RedWagon"))
			//state.DDIIPubKey = ddiiname[:]
			state.StateHash = managed.Hash{}.Bytes()
			state.Entry = sub.GetData()
			//key := app.mm.PT.StoreState(sub.GetChainid(),sha256.Sum256(sub.GetData()))
			key := sub.GetChainid() //only temporary...

			err = app.mmdb.Put("Entries-Debug","", key, sub.GetData())
			if err != nil {
				ret.Code = code.CodeTypeEncodingError
				ret.Info = fmt.Sprintf("Error submitting entry to database for chain %X",sub.GetChainid())
				ret.GasWanted = 0
			}
		default:
			ret.Code = code.CodeTypeEncodingError
			ret.Info = fmt.Sprintf("Unknown message type %v on address %v \n",sub.Type, sub.Identitychain)
			return ret
		}

 */
		if err != nil {
			ret.Code = 2
			ret.GasWanted = 0
			return ret
		}
	}

	return response
}


///ABCI / block calls
///   BeginBlock
///   [CheckTx]
///   [DeliverTx]
///   EndBlock <---
///   Commit


// Update the validator set
func (app *AccumulatorVMApplication) EndBlock(req abcitypes.RequestEndBlock) (resp abcitypes.ResponseEndBlock) {
	// Select our leader who will initiate consensus on dbvc chain.
	//resp.ConsensusParamUpdates
	//for _, ev := range req.ByzantineValidators {
	//	if ev.Type == types.EvidenceType_DUPLICATE_VOTE {
	//		addr := string(ev.Validator.Address)
	//		if pubKey, ok := app.valAddrToPubKeyMap[addr]; ok {
	//			app.updateValidator(types.ValidatorUpdate{
	//				PubKey: pubKey,
	//				Power:  ev.Validator.Power - 1,
	//			})
	//			app.logger.Info("Decreased val power by 1 because of the equivocation",
	//				"val", addr)
	//		} else {
	//			app.logger.Error("Wanted to punish val, but can't find it",
	//				"val", addr)
	//		}
	//	}
	//}

	return abcitypes.ResponseEndBlock{}//ValidatorUpdates: app.ValUpdates}
}
///ABCI / block calls
///   BeginBlock
///   [CheckTx]
///   [DeliverTx]
///   EndBlock
///   Commit <---

//Commit instructs the application to persist the new state.
func (app *AccumulatorVMApplication) Commit() (resp abcitypes.ResponseCommit) {
	//end the current batch of transactions in the Stateful Merkle Tree

	mdroot := app.writeStates()

	resp.Data = mdroot
	//saveDBlock



	//I think we need to get this from the bpt
	//app.bpt.Bpt.Root.Hash
	//if we have no transactions this block then don't publish anything
	if app.amLeader && app.txct > 0 {

		//now we create a synthetic transaction and publish to the directory block validator
		//bve := BVCEntry{}
		//bve.Version = 1
		//bve.BVCHeight = app.Height
		//bve.DDII = make([]byte, len("placeholder")+1)
		//copy(bve.DDII, []byte(string("placeholder")))
		//bve.Timestamp = uint64(valacctypes.GetCurrentTimeStamp())
		//copy(bve.MDRoot.Bytes(), mdroot)
		//

		dbvc := validator.ResponseValidateTX{}
		dbvc.Submissions = make([]pb.Submission,1)
		dbvc.Submissions[0].Instruction = 0
		chainadi := string("dbvc")
		chainid, _ := validator.BuildChainIdFromAdi(&chainadi)
        //chainaddr, _ := smt.BytesUint64(chainid)
		dbvc.Submissions[0].Identitychain = chainid //1 is the chain id of the DBVC
		dbvc.Submissions[0].Chainid = chainid

		dbvc.Submissions[0].Instruction = pb.AccInstruction_Data_Entry //this may be irrelevant...
		dbvc.Submissions[0].Param1 = 0
		dbvc.Submissions[0].Param2 = 0
		//bvedata,err := bve.MarshalBinary()
		//if err != nil {
		//	///shouldn't get here.
		//	fmt.Printf("Shouldn't get here... invalid BVE marshal")
		//	return abcitypes.ResponseCommit{}
		//}
		//dbvc.Submissions[0].Data = bvedata

		//send to router.
		//app.processValidatedSubmissionRequest(&dbvc)
	}


	//this will truncate what tendermint stores since we only care about current state
	if app.RetainBlocks > 0 && app.Height >= app.RetainBlocks {
		//todo: add this back when done with debugging.
		//resp.RetainHeight = app.Height - app.RetainBlocks + 1
	}


	//appHash := make([]byte, 8)
	//binary.PutVarint(appHash, app.state.Size)
	app.state.Size += int64(app.txct)
	app.state.AppHash = mdroot
	app.state.Height++
	saveState(app.state)

	duration := time.Since(app.timer)
	fmt.Printf("TPS: %d in %f for %f\n", app.txct, duration.Seconds(), float64(app.txct) / duration.Seconds() )
	//return resp
	return resp
}


//------------------------


func (app *AccumulatorVMApplication) ListSnapshots(
	req abcitypes.RequestListSnapshots) abcitypes.ResponseListSnapshots {
	req.ProtoMessage()
	return abcitypes.ResponseListSnapshots{}
}

func (app *AccumulatorVMApplication) LoadSnapshotChunk(
	req abcitypes.RequestLoadSnapshotChunk) abcitypes.ResponseLoadSnapshotChunk {
	//req.Height
	//resp := abcitypes.ResponseLoadSnapshotChunk{}
	//need to get a block of data between markers.
	//resp.Chunk = app.mm.GetState(req.Height)
	return abcitypes.ResponseLoadSnapshotChunk{}
}

func (app *AccumulatorVMApplication) OfferSnapshot(
	req abcitypes.RequestOfferSnapshot) abcitypes.ResponseOfferSnapshot {
	return abcitypes.ResponseOfferSnapshot{Result: abcitypes.ResponseOfferSnapshot_ABORT}
}

func (app *AccumulatorVMApplication) ApplySnapshotChunk(

	req abcitypes.RequestApplySnapshotChunk) abcitypes.ResponseApplySnapshotChunk {
	return abcitypes.ResponseApplySnapshotChunk{Result: abcitypes.ResponseApplySnapshotChunk_ABORT}
}


// when the client wants to know whenever a particular key/value exist, it will call Tendermint Core RPC /abci_query endpoint
func (app *AccumulatorVMApplication) Query(reqQuery abcitypes.RequestQuery) (resQuery abcitypes.ResponseQuery) {
	resQuery.Key = reqQuery.Data

    q := pb.AccQuery{}
    err :=  proto.Unmarshal(reqQuery.Data,&q)
    if err != nil {
    	resQuery.Info = fmt.Sprintf("Requst is not an Accumulate Query\n")
    	resQuery.Code = code.CodeTypeUnauthorized
    	return resQuery
	}
	fmt.Printf("Query URI: %s", q.Query)

	///implement lazy sync calls. If a node falls behind it needs to have several query calls
	///1 get current height
	///2 get block data for height X
	///3 get block data for given hash

	/*
	err := app.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(reqQuery.Data)
		if err != nil && err != badger.ErrKeyNotFound {
			return err
		}
		if err == badger.ErrKeyNotFound {
			resQuery.Log = "does not exist"
		} else {
			return item.Value(func(val []byte) error {
				resQuery.Log = "exists"
				resQuery.Value = val
				return nil
			})
		}
		return nil
	})
	if err != nil {
		panic(err)
	}

	 */
	return
}


func (app *AccumulatorVMApplication) GetName() string {
	return app.config.ChainID()
}
func (app *AccumulatorVMApplication) Wait() {
    app.waitgroup.Wait()
}

func dialerFunc(ctx context.Context, addr string) (net.Conn, error) {
	return tmnet.Connect(addr)
}

func (app *AccumulatorVMApplication) Start() (*nm.Node, error) {

	// create logger
	logger := log.NewTMLogger(log.NewSyncWriter(os.Stdout))
	var err error
	logger, err = tmflags.ParseLogLevel(app.config.LogLevel, logger, cfg.DefaultLogLevel())
	if err != nil {
		return nil, fmt.Errorf("failed to parse log level: %w", err)
	}

	// read private validator
	pv := privval.LoadFilePV(
		app.config.PrivValidatorKeyFile(),
		app.config.PrivValidatorStateFile(),
	)

	// read node key
	nodeKey, err := p2p.LoadNodeKey(app.config.NodeKeyFile())
	if err != nil {
		return nil, fmt.Errorf("failed to load node's key: %w", err)
	}

	fmt.Printf("Public Address: 0x%X\n", pv.Key.PubKey.Address())

	//sk := ed25519.PrivateKey{}

	app.Key = pv.Key //.PrivKey
	app.Address = make([]byte, len(pv.Key.PubKey.Address()))
	copy(app.Address, pv.Key.PubKey.Address())

	//this should be done outside of here...
	// create node
	node, err := nm.NewNode(
		app.config,
		pv,
		nodeKey,
		proxy.NewLocalClientCreator(app),
		nm.DefaultGenesisDocProviderFunc(app.config),
		nm.DefaultDBProvider,
		nm.DefaultMetricsProvider(app.config.Instrumentation),
		logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create new Tendermint node: %w", err)
	}
	//node.


	fmt.Println("Accumulate Start" + app.config.ChainID() )

    err = node.Start()
    if err != nil {
    	panic(err)
	}

	WaitForGRPC(app.config.RPC.GRPCListenAddress)
	WaitForRPC(app.config.RPC.ListenAddress)

	app.LocalClient = local.New(node)


	//makeGRPCServer(app,app.Accrpcaddr )//app.config.RPC.GRPCListenAddress)

	//return &api, nil


	client:= GetGRPCClient(app.config.RPC.GRPCListenAddress) //makeGRPCClient(app.Accrpcaddr)//app.config.RPC.GRPCListenAddress)

	app.APIClient = client

	//s := node.Listeners()
	defer func() {
		node.Stop()
		node.Wait()
		fmt.Println("Tendermint Stopped")
	}()

    //time.Sleep(10000*time.Millisecond)
	if node.IsListening() {
		fmt.Print("node is listening")
	}
	app.waitgroup.Done()
	node.Wait()

	return node,nil
}


// add, update, or remove a validator
func (app *AccumulatorVMApplication) updateValidator(v abcitypes.ValidatorUpdate) abcitypes.ResponseDeliverTx {

	pubkey, _ := cryptoenc.PubKeyFromProto(v.PubKey)

	fmt.Printf("Val Pub Key 0x%X\n", pubkey.Address())
/*
	if err != nil {
		panic(fmt.Errorf("can't decode public key: %w", err))
	}
	//key := []byte("val:" + string(pubkey.Bytes()))
	if v.Power == 0 {
		// remove validator
		_, found := app.tmvalidators[string(pubkey.Address())]// app.app.state.db.Has(key)
		if !found {
			pubStr := base64.StdEncoding.EncodeToString(pubkey.Bytes())
			return abcitypes.ResponseDeliverTx{
				Code: code.CodeTypeUnauthorized,
				Log:  fmt.Sprintf("Cannot remove non-existent validator %s", pubStr)}
		}
//		if !hasKey
		//if err = app.app.state.db.Delete(key); err != nil {
		//	panic(err)
		//}
		delete(app.tmvalidators, string(pubkey.Address()))
	} else {
		// add or update validator
		//value := bytes.NewBuffer(make([]byte, 0))
		//if err := types.WriteMessage(&v, value); err != nil {
		//	return types.ResponseDeliverTx{
		//		Code: code.CodeTypeEncodingError,
		//		Log:  fmt.Sprintf("Error encoding validator: %v", err)}
		//}
		//if err = app.app.state.db.Set(key, value.Bytes()); err != nil {
		//	panic(err)
		//}
		app.tmvalidators[string(pubkey.Address())] = pubkey
	}
*/

	// we only update the changes array if we successfully updated the tree
	//app.ValUpdates = append(app.ValUpdates, v)

	return abcitypes.ResponseDeliverTx{Code: code.CodeTypeOK}
}
