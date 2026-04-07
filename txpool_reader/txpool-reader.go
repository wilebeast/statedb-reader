package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/txpool/legacypool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/pathdb"
)

type localChain struct {
	config *params.ChainConfig
	db     ethdb.Database
	triedb *triedb.Database
	head   *types.Header
	scheme string
}

func (c *localChain) Config() *params.ChainConfig {
	return c.config
}

func (c *localChain) CurrentBlock() *types.Header {
	return c.head
}

func (c *localChain) GetBlock(hash common.Hash, number uint64) *types.Block {
	return rawdb.ReadBlock(c.db, hash, number)
}

func (c *localChain) StateAt(root common.Hash) (*state.StateDB, error) {
	statedb, err := state.New(root, state.NewDatabase(c.triedb, nil))
	if err != nil && c.scheme == rawdb.PathScheme {
		return state.New(root, state.NewHistoricDatabase(c.triedb, nil))
	}
	return statedb, err
}

func openChainDatabase(chainDbPath string) (ethdb.Database, error) {
	var (
		kvdb ethdb.KeyValueStore
		err  error
	)
	switch engine := rawdb.PreexistingDatabase(chainDbPath); engine {
	case rawdb.DBPebble:
		kvdb, err = pebble.New(chainDbPath, 128, 1024, "txpool-reader", true)
	case rawdb.DBLeveldb:
		return nil, fmt.Errorf("leveldb chain database is not supported by this reader without adding github.com/syndtr/goleveldb")
	case "":
		return nil, fmt.Errorf("no geth database found at %s", chainDbPath)
	default:
		return nil, fmt.Errorf("unsupported geth database engine %q", engine)
	}
	if err != nil {
		return nil, err
	}
	db, err := rawdb.Open(kvdb, rawdb.OpenOptions{
		Ancient:          filepath.Join(chainDbPath, "ancient"),
		MetricsNamespace: "txpool-reader",
		ReadOnly:         true,
	})
	if err != nil {
		kvdb.Close()
		return nil, err
	}
	return db, nil
}

func trieConfig(db ethdb.Database, dataDir string) (*triedb.Config, string, error) {
	scheme, err := rawdb.ParseStateScheme("", db)
	if err != nil {
		return nil, "", err
	}
	if scheme == rawdb.HashScheme {
		return triedb.HashDefaults, scheme, nil
	}
	if scheme != rawdb.PathScheme {
		return nil, "", fmt.Errorf("unsupported state scheme: %s", scheme)
	}
	config := *pathdb.ReadOnly
	journalDir, err := filepath.Abs(filepath.Join(dataDir, "triedb"))
	if err != nil {
		return nil, "", err
	}
	config.JournalDirectory = journalDir
	fmt.Printf("📰 trie journal 目录: %s\n", journalDir)
	return &triedb.Config{PathDB: &config}, scheme, nil
}

func loadJournal(path string) ([]*types.Transaction, error) {
	input, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer input.Close()

	var txs []*types.Transaction
	stream := rlp.NewStream(input, 0)
	for {
		tx := new(types.Transaction)
		if err := stream.Decode(tx); err != nil {
			if errors.Is(err, io.EOF) {
				return txs, nil
			}
			return txs, err
		}
		txs = append(txs, tx)
	}
}

func main() {
	// --- 配置区域 ---
	dataDir := "./devdata/geth" // 注意这里指向 geth 目录，而不是 chaindata
	// ----------------

	// 1. 打开底层数据库 (我们需要它来读取 StateDB，以验证交易池中的交易是否合法)
	chainDbPath := filepath.Join(dataDir, "chaindata")
	chainDb, err := openChainDatabase(chainDbPath)
	if err != nil {
		fmt.Printf("❌ 无法打开链数据库: %v\n", err)
		os.Exit(1)
	}
	defer chainDb.Close()

	// 2. 获取当前的 ChainConfig (主网、测试网配置等)
	// 这一步是为了确保我们能正确解析交易类型
	genesisHash := rawdb.ReadCanonicalHash(chainDb, 0)
	if genesisHash == (common.Hash{}) {
		fmt.Println("❌ 无法读取创世块哈希")
		os.Exit(1)
	}

	// 尝试读取配置，如果读不到就用默认的主网配置（在 dev 模式下通常没问题）
	chainConfig := rawdb.ReadChainConfig(chainDb, genesisHash)
	if chainConfig == nil {
		fmt.Println("⚠️ 警告: 未找到链配置，使用默认配置")
		chainConfig = params.MainnetChainConfig
	}

	// 3. 获取最新的 StateDB
	// 交易池需要 StateDB 来检查 Nonce 和余额
	head := rawdb.ReadHeadHeader(chainDb)
	if head == nil {
		fmt.Println("❌ 无法读取最新区块头")
		os.Exit(1)
	}
	trieDBConfig, scheme, err := trieConfig(chainDb, dataDir)
	if err != nil {
		fmt.Printf("❌ 无法检测状态数据库方案: %v\n", err)
		os.Exit(1)
	}
	trieDB := triedb.NewDatabase(chainDb, trieDBConfig)
	defer trieDB.Close()
	chain := &localChain{
		config: chainConfig,
		db:     chainDb,
		triedb: trieDB,
		head:   head,
		scheme: scheme,
	}
	_, err = chain.StateAt(head.Root)
	if err != nil {
		fmt.Printf("❌ 无法创建 StateDB: %v\n", err)
		os.Exit(1)
	}

	// 4. 初始化交易池配置
	// 这里我们模拟 Geth 的默认配置
	journalPath := filepath.Join(dataDir, legacypool.DefaultConfig.Journal)
	poolConfig := legacypool.DefaultConfig
	poolConfig.Journal = journalPath

	// 5. 创建交易池实例
	// 这是一个内存对象；geth v1.17 的本地交易持久化在 transactions.rlp 中。
	txPool := legacypool.New(poolConfig, chain)
	reserver := txpool.NewReservationTracker().NewHandle(0)
	if err := txPool.Init(poolConfig.PriceLimit, head, reserver); err != nil {
		fmt.Printf("❌ 无法初始化交易池: %v\n", err)
		os.Exit(1)
	}
	defer txPool.Close()

	journalTxs, err := loadJournal(journalPath)
	if err != nil {
		fmt.Printf("❌ 无法读取交易池日志: %v\n", err)
		os.Exit(1)
	}
	if len(journalTxs) > 0 {
		errs := txPool.Add(journalTxs, true)
		dropped := 0
		for _, err := range errs {
			if err != nil {
				dropped++
			}
		}
		fmt.Printf("📄 已从 journal 读取 %d 笔交易，丢弃 %d 笔无效交易\n", len(journalTxs), dropped)
	}

	// 注意：在真实的 Geth 运行中，交易池是动态更新的。
	// 在这里，我们主要关注它从磁盘加载了哪些“旧”交易。

	fmt.Printf("✅ 交易池已初始化\n")
	fmt.Printf("📂 交易池 journal: %s\n", journalPath)

	// 6. 获取待处理交易 (Pending Transactions)
	// Pending 指的是 Nonce 正确，随时可以被打包的交易
	pending, _ := txPool.Pending(txpool.PendingFilter{})

	count := 0
	signer := types.LatestSigner(chainConfig)
	for addr, txs := range pending {
		fmt.Printf("\n--- 🧑 账户: %s ---\n", addr.Hex())
		for _, lazyTx := range txs {
			tx := lazyTx.Resolve()
			if tx == nil {
				continue
			}
			count++
			// 解析交易详情
			from, _ := types.Sender(signer, tx)
			to := tx.To()
			toStr := "合约创建"
			if to != nil {
				toStr = to.Hex()
			}

			fmt.Printf("   💸 交易哈希: %x...\n", tx.Hash().Bytes()[:4])
			fmt.Printf("      发送者: %s\n", from.Hex())
			fmt.Printf("      接收者: %s\n", toStr)
			fmt.Printf("      金额: %s Wei\n", tx.Value().String())
			fmt.Printf("      Nonce: %d\n", tx.Nonce())
			fmt.Printf("      Gas价格: %s\n", tx.GasPrice().String())
		}
	}

	if count == 0 {
		fmt.Println("\n💤 交易池是空的（或者没有待处理的交易）。")
		fmt.Println("   试着在 Geth 控制台发送几笔交易，但不要打包（或者重启 Geth 让它把交易存盘）。")
	} else {
		fmt.Printf("\n🎉 成功从交易池持久化文件中读取了 %d 笔交易！\n", count)
	}
}
