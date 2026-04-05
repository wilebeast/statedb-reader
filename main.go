package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/pathdb"
	"github.com/holiman/uint256"
)

func main() {
	// --- 配置区域 ---
	dataDir := "./devdata/geth/chaindata"
	accountAddress := "0x71562b71999873db5b286df957af199ec94617f7"
	// ----------------

	// 1. 打开底层 Pebble 数据库，并附加 ancient freezer 数据。
	kvdb, err := pebble.New(dataDir, 128, 128, "", true)
	if err != nil {
		fmt.Printf("❌ 无法打开 Pebble 数据库 %s: %v\n", dataDir, err)
		os.Exit(1)
	}
	defer kvdb.Close()

	db, err := rawdb.Open(kvdb, rawdb.OpenOptions{
		Ancient:  filepath.Join(dataDir, "ancient"),
		ReadOnly: true,
	})
	if err != nil {
		fmt.Printf("❌ 无法打开链数据库 %s: %v\n", dataDir, err)
		os.Exit(1)
	}
	defer db.Close()
	fmt.Printf("✅ 数据库已打开: %s\n", dataDir)

	// 2. 获取最新区块头，确定查询所对应的世界状态根。
	head := rawdb.ReadHeadHeader(db)
	if head == nil {
		fmt.Println("❌ 无法读取区块头，数据库可能为空")
		os.Exit(1)
	}
	fmt.Printf("📦 当前最新区块高度: %d\n", head.Number.Uint64())
	fmt.Printf("📦 当前状态根哈希 (StateRoot): %x\n", head.Root)

	// 3. 根据数据库实际使用的状态方案创建 trie/state 访问层。
	scheme, err := rawdb.ParseStateScheme("", db)
	if err != nil {
		fmt.Printf("❌ 无法识别状态存储方案: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("🗂️ 状态存储方案: %s\n", scheme)
	trieConfig, err := trieConfigForScheme(scheme, dataDir)
	if err != nil {
		fmt.Printf("❌ 无法构造 trie 配置: %v\n", err)
		os.Exit(1)
	}
	trieDB := triedb.NewDatabase(db, trieConfig)
	stateDB, primaryErr := state.New(head.Root, state.NewDatabase(trieDB, nil))
	var historicErr error
	if primaryErr != nil && scheme == rawdb.PathScheme {
		// Path-based state may keep the requested root only in historical state storage.
		stateDB, historicErr = state.New(head.Root, state.NewHistoricDatabase(trieDB, nil))
	}
	if primaryErr != nil {
		fmt.Printf("⚠️ 常规 StateDB 打开失败: %v\n", primaryErr)
	}
	if historicErr != nil {
		fmt.Printf("⚠️ 历史 StateDB 回退失败: %v\n", historicErr)
	}
	if primaryErr != nil && historicErr != nil {
		fmt.Println("❌ 无法创建 StateDB: 常规读取与历史读取都失败")
		os.Exit(1)
	}
	if primaryErr != nil && scheme != rawdb.PathScheme {
		fmt.Printf("❌ 无法创建 StateDB: %v\n", primaryErr)
		os.Exit(1)
	}

	// 4. 读取目标账户。
	addr := common.HexToAddress(accountAddress)
	if !stateDB.Exist(addr) {
		fmt.Printf("❌ 在 StateDB 中未找到账户 %s\n", accountAddress)
		fmt.Println("   请确认地址是否正确，以及该账户在该状态根下确实存在。")
		os.Exit(1)
	}

	balance := new(uint256.Int).Set(stateDB.GetBalance(addr))
	nonce := stateDB.GetNonce(addr)
	storageRoot := stateDB.GetStorageRoot(addr)
	codeHash := stateDB.GetCodeHash(addr)
	code := stateDB.GetCode(addr)

	// 5. 打印账户信息。
	fmt.Println("\n--- 🧑 账户信息 (来自 StateDB) ---")
	fmt.Printf("地址: %s\n", addr.Hex())
	fmt.Printf("余额 (Wei): %s\n", balance.String())
	fmt.Printf("Nonce: %d\n", nonce)
	fmt.Printf("存储根: %x\n", storageRoot)
	fmt.Printf("代码哈希: %x\n", codeHash)
	fmt.Printf("代码长度: %d bytes\n", len(code))

	// 6. 组装共识层账户对象并输出其原始 RLP。
	account := &types.StateAccount{
		Nonce:    nonce,
		Balance:  balance,
		Root:     storageRoot,
		CodeHash: codeHash.Bytes(),
	}
	rlpData, err := rlp.EncodeToBytes(account)
	if err != nil {
		fmt.Printf("❌ RLP 编码失败: %v\n", err)
	} else {
		fmt.Printf("原始 RLP (Hex): %x\n", rlpData)
	}

	// 7. 可选：用 RPC 结果交叉验证。
	fmt.Println("\n--- ✅ 验证 ---")
	fmt.Println("如果你的 Geth 节点正在运行，可以在控制台用以下命令验证：")
	fmt.Printf("eth.getBalance(\"%s\")\n", accountAddress)
	fmt.Printf("eth.getTransactionCount(\"%s\")\n", accountAddress)
}

func trieConfigForScheme(scheme, dataDir string) (*triedb.Config, error) {
	switch scheme {
	case rawdb.PathScheme:
		cfg := *pathdb.ReadOnly
		journalDir, err := filepath.Abs(filepath.Join(filepath.Dir(dataDir), "triedb"))
		if err != nil {
			return nil, err
		}
		cfg.JournalDirectory = journalDir
		fmt.Printf("📰 trie journal 目录: %s\n", journalDir)
		return &triedb.Config{PathDB: &cfg}, nil
	case rawdb.HashScheme:
		return triedb.HashDefaults, nil
	default:
		return nil, fmt.Errorf("unsupported scheme: %s", scheme)
	}
}
