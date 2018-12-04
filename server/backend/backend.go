package backend

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"time"

	"errors"
	"regexp"

	"github.com/gochain-io/explorer/server/models"
	"github.com/gochain-io/gochain/common"
	"github.com/gochain-io/gochain/common/compiler"
	"github.com/gochain-io/gochain/core/types"
	"github.com/gochain-io/gochain/goclient"
	"github.com/rs/zerolog/log"
	"net/url"
)

const RECAPTCHA_URL = "https://www.google.com/recaptcha/api/siteverify"

type Backend struct {
	mongo             *MongoBackend
	goClient          *goclient.Client
	extendedEthClient *EthRPC
	tokenBalance      *TokenBalance
	reCaptchaSecret   string
}

func NewBackend(mongoUrl, rpcUrl, dbName string, reCaptchaSecret string) *Backend {
	client, err := goclient.Dial(rpcUrl)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot create eth client")
	}
	exClient := NewEthClient(rpcUrl)
	mongoBackend := NewMongoClient(mongoUrl, rpcUrl, dbName)
	importer := new(Backend)
	importer.goClient = client
	importer.extendedEthClient = exClient
	importer.mongo = mongoBackend
	importer.tokenBalance = NewTokenBalanceClient(rpcUrl)
	importer.reCaptchaSecret = reCaptchaSecret
	return importer
}

//METHODS USED IN API
func (self *Backend) BalanceAt(address, block string) (*big.Int, error) {
	return self.extendedEthClient.ethGetBalance(address, block)
}
func (self *Backend) TotalSupply() (*big.Int, error) {
	return self.extendedEthClient.ethTotalSupply()
}
func (self *Backend) CirculatingSupply() (*big.Int, error) {
	return self.extendedEthClient.circulatingSupply()
}
func (self *Backend) GetStats() *models.Stats {
	return self.mongo.getStats()
}
func (self *Backend) GetRichlist(skip, limit int) []*models.Address {
	return self.mongo.getRichlist(skip, limit)
}
func (self *Backend) GetAddressByHash(hash string) *models.Address {
	return self.mongo.getAddressByHash(common.HexToAddress(hash).Hex())
}
func (self *Backend) GetTransactionByHash(hash string) *models.Transaction {
	return self.mongo.getTransactionByHash(hash)
}
func (self *Backend) GetTransactionList(address string, skip, limit int) []*models.Transaction {
	return self.mongo.getTransactionList(common.HexToAddress(address).Hex(), skip, limit)
}
func (self *Backend) GetTokenHoldersList(contractAddress string, skip, limit int) []*models.TokenHolder {
	return self.mongo.getTokenHoldersList(common.HexToAddress(contractAddress).Hex(), skip, limit)
}
func (self *Backend) GetInternalTransactionsList(contractAddress string, skip, limit int) []*models.InternalTransaction {
	return self.mongo.getInternalTransactionsList(common.HexToAddress(contractAddress).Hex(), skip, limit)
}
func (self *Backend) GetContract(contractAddress string) *models.Contract {
	return self.mongo.getContract(common.HexToAddress(contractAddress).Hex())
}
func (self *Backend) GetLatestsBlocks(skip, limit int) []*models.LightBlock {
	return self.mongo.getLatestsBlocks(skip, limit)
}
func (self *Backend) GetBlockTransactionsByNumber(blockNumber int64, skip, limit int) []*models.Transaction {
	return self.mongo.getBlockTransactionsByNumber(blockNumber, skip, limit)
}

func (self *Backend) GetBlockByNumber(number int64) *models.Block {
	block := self.mongo.getBlockByNumber(number)
	if block == nil {
		log.Info().Int64("blockNumber", number).Msg("cannot get block from db, importing it")
		blockEth, err := self.goClient.BlockByNumber(context.Background(), big.NewInt(number))
		if err != nil {
			log.Info().Err(err).Int64("blockNumber", number).Msg("cannot get block from eth and db")
			return nil
		}
		block = self.ImportBlock(blockEth)
	}
	return block
}

func (self *Backend) GetBlockByHash(hash string) *models.Block {
	return self.mongo.getBlockByHash(hash)
}

func (self *Backend) VerifyContract(contractData *models.Contract) (*models.Contract, error) {
	contract := self.GetContract(contractData.Address)
	if contract == nil {
		err := errors.New("contract with given address not found")
		return nil, err
	}
	if contract.Valid == true {
		err := errors.New("contract with given address is already verified")
		return nil, err
	}
	compileData, err := compiler.CompileSolidityString("solc", contractData.SourceCode)
	if err != nil {
		err := errors.New("error occurred while compiling source code")
		return nil, err
	}
	// compiler gives map with keys starting with <stdin>:
	key := "<stdin>:" + contractData.ContractName
	if _, ok := compileData[key]; !ok {
		err := errors.New("invalid contract name")
		return nil, err
	}
	if compileData[key].RuntimeCode == "" {
		err := errors.New("contract binary is empty")
		return nil, err
	}
	// removing '0x' from start
	sourceBin := compileData[key].RuntimeCode[2:]
	// removing metadata hash from binary
	reg := regexp.MustCompile(`00a165627a7a72305820.*0029$`)
	sourceBin = reg.ReplaceAllString(sourceBin, ``)
	contractBin := reg.ReplaceAllString(contract.Bytecode, ``)
	if sourceBin == contractBin {
		contract.Valid = true
		contract.Optimization = true
		contract.ContractName = contractData.ContractName
		contract.SourceCode = compileData[key].Info.Source
		contract.CompilerVersion = compileData[key].Info.CompilerVersion
		contract.UpdatedAt = time.Now()
		result := self.mongo.updateContract(contract)
		if !result {
			err := errors.New("error occurred while processing data")
			return nil, err
		}
		return contract, nil
	} else {
		err := errors.New("the compiled result does not match the input creation bytecode located at " + contractData.Address)
		return nil, err
	}
}

func (self *Backend) GetCompilerVersion() (string, error) {
	result, err := compiler.SolidityVersion("solc")
	if err != nil {
		err := errors.New("error occurred while processing")
		return "", err
	}
	versionRegexp := regexp.MustCompile(`([0-9]+)\.([0-9]+)\.([0-9]+)\+commit\.[^.]*`)
	longVersion := versionRegexp.FindStringSubmatch(result.FullVersion)
	return longVersion[0], nil
}

//METHODS USED IN GRABBER

func (self *Backend) UpdateStats() {
	self.mongo.updateStats()
}
func (self *Backend) GenesisAlloc() (*big.Int, []string, error) {
	return self.extendedEthClient.genesisAlloc()
}
func (self *Backend) GetTokenBalance(contract, wallet string) (*TokenHolderDetails, error) {
	return self.tokenBalance.GetTokenHolderDetails(contract, wallet)
}

func (self *Backend) GetTokenDetails(contract string) (*TokenDetails, error) {
	return self.tokenBalance.GetTokenDetails(contract)
}

func (self *Backend) GetInternalTransactions(address string) []TransferEvent {
	return self.tokenBalance.getInternalTransactions(address)
}
func (self *Backend) ImportBlock(block *types.Block) *models.Block {
	return self.mongo.importBlock(block)
}
func (self *Backend) NeedReloadBlock(blockNumber int64) bool {
	return self.mongo.needReloadBlock(blockNumber)
}
func (self *Backend) TransactionsConsistent(blockNumber int64) bool {
	return self.mongo.transactionsConsistent(blockNumber)
}
func (self *Backend) GetActiveAdresses(fromDate time.Time, onlyContracts bool) []*models.ActiveAddress {
	var selectedAddresses []*models.ActiveAddress
	for _, address := range self.mongo.getActiveAdresses(fromDate) {
		if onlyContracts == self.mongo.isContract(address.Address) {
			selectedAddresses = append(selectedAddresses, address)
		}
	}
	return selectedAddresses
}
func (self *Backend) ImportAddress(address string, balance *big.Int, token *TokenDetails, contract, go20 bool) *models.Address {
	return self.mongo.importAddress(address, balance, token, contract, go20)
}
func (self *Backend) ImportTokenHolder(contractAddress, tokenHolderAddress string, token *TokenHolderDetails) *models.TokenHolder {
	return self.mongo.importTokenHolder(contractAddress, tokenHolderAddress, token)
}
func (self *Backend) ImportInternalTransaction(contractAddress string, transferEvent TransferEvent) *models.InternalTransaction {
	return self.mongo.importInternalTransaction(contractAddress, transferEvent)
}
func (self *Backend) ImportContract(contractAddress string, byteCode string) *models.Contract {
	return self.mongo.importContract(contractAddress, byteCode)
}

func (self *Backend) VerifyReCaptcha(token string, action string, remoteIp string) error {
	if self.reCaptchaSecret == "" {
		return nil
	}
	/*payload := &models.ReCaptchaRequest{
		Secret:   self.reCaptchaSecret,
		Response: token,
		RemoteIp: remoteIp,
	}
	var bytesRepresentation bytes.Buffer
	if err := json.NewEncoder(&bytesRepresentation).Encode(payload); err != nil {
		log.Fatal().Err(err).Msg("error occurred during encoding recaptcha payload")
		err := errors.New("error occurred during processing your request. please try again")
		return err
	}
	resp, err := http.Post(RECAPTCHA_URL, "application/json; charset=utf-8", &bytesRepresentation)*/
	params := url.Values{}
	params.Add("secret", self.reCaptchaSecret)
	params.Add("response", token)
	if remoteIp != "" {
		params.Add("remoteip", remoteIp)
	}
	resp, err := http.PostForm(RECAPTCHA_URL, params)
	if err != nil {
		log.Fatal().Err(err).Msg("error occurred during making recaptcha request")
		err := errors.New("error occurred during processing your request. please try again")
		return err
	}
	var result *models.ReCaptchaResponse
	json.NewDecoder(resp.Body).Decode(&result)
	// resp.Body.Close()
	if result.Success == false {
		err := errors.New("error occurred during anti-bot checking. please try again")
		return err
	}
	if result.Score < 0.5 {
		err := errors.New("not handling bot request")
		return err
	}
	return nil
}

// HeaderByNumber
// BlockByNumber
// BalanceAt
// CodeAt