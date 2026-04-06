package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/pathdb"
	"github.com/holiman/uint256"
)

func main() {
	// --- 配置区域 ---
	dataDir := "./devdata/geth/chaindata"
	accountAddress := "0x71562b71999873db5b286df957af199ec94617f7"
	dumpPrefix := ""
	dumpLimit := 150
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

	// 7. 打印一份可手工比对的验证信息，帮助理解 stateRoot -> account -> codeHash 的关系。
	if err := printManualVerification(db, trieDB, head.Root, scheme, addr, account, rlpData, code); err != nil {
		fmt.Printf("⚠️ 手工验证辅助信息生成失败: %v\n", err)
	}

	// 8. 直接遍历 Pebble 中的原始 key-value。
	printPebbleDump(kvdb, dumpPrefix, dumpLimit)

	// 9. 可选：用 RPC 结果交叉验证。
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

func printManualVerification(diskdb ethdb.KeyValueReader, trieDB *triedb.Database, stateRoot common.Hash, scheme string, addr common.Address, account *types.StateAccount, accountRLP []byte, code []byte) error {
	fmt.Println("\n--- 🔎 手工验证辅助 ---")

	addrHash := crypto.Keccak256Hash(addr.Bytes())
	accountRLPHash := crypto.Keccak256Hash(accountRLP)
	codeHashFromCode := crypto.Keccak256Hash(code)

	fmt.Printf("账户地址: %s\n", addr.Hex())
	fmt.Printf("账户哈希 keccak(address): %x\n", addrHash)
	fmt.Printf("账户 RLP 的 keccak: %x\n", accountRLPHash)
	fmt.Printf("代码键 (Pebble): 63%x\n", account.CodeHash)
	fmt.Printf("代码 keccak(code): %x\n", codeHashFromCode)
	fmt.Printf("代码哈希匹配: %t\n", codeHashFromCode == common.BytesToHash(account.CodeHash))

	codeBlob := rawdb.ReadCode(diskdb, common.BytesToHash(account.CodeHash))
	fmt.Printf("代码键读取成功: %t\n", len(codeBlob) > 0 || len(code) == 0)

	statedb := state.NewDatabase(trieDB, nil)
	accountTrie, err := statedb.OpenTrie(stateRoot)
	if err != nil {
		return err
	}

	proofDB := rawdb.NewMemoryDatabase()
	if err := accountTrie.Prove(addr.Bytes(), proofDB); err != nil {
		return err
	}
	proofValue, err := trie.VerifyProof(stateRoot, addr.Bytes(), proofDB)
	if err != nil {
		return err
	}
	fmt.Printf("proof 校验成功: %t\n", len(proofValue) > 0)
	fmt.Printf("proof 返回的账户值 == 本地账户 RLP: %t\n", string(proofValue) == string(accountRLP))

	fmt.Printf("状态方案: %s\n", scheme)
	if scheme == rawdb.HashScheme {
		fmt.Println("说明: hash 方案下，proof DB 的 key 就是节点哈希；Pebble 中通常也是直接用节点哈希作 key。")
	} else {
		fmt.Println("说明: path 方案下，Pebble 中真实 key 是 A<path> / O<accountHash><path>；proof DB 里仍然按节点哈希组织，便于你看 root 如何逐层校验。")
	}

	fmt.Println("账户 proof 节点列表:")
	it := proofDB.NewIterator(nil, nil)
	defer it.Release()
	for i := 0; it.Next(); i++ {
		nodeHash := common.BytesToHash(it.Key())
		nodeBlob := common.CopyBytes(it.Value())
		fmt.Printf("  [%d] hash=%x len=%d keccak(blob)=%x\n", i, nodeHash, len(nodeBlob), crypto.Keccak256Hash(nodeBlob))
		fmt.Printf("      blob=%x\n", nodeBlob)
	}
	return nil
}

func printPebbleDump(kvdb ethdb.Iteratee, prefix string, limit int) {
	fmt.Println("\n--- 🪨 Pebble 原始 KV Dump ---")

	var prefixBytes []byte
	if prefix != "" {
		prefixBytes = []byte(prefix)
	}
	if limit <= 0 {
		limit = 50
	}
	fmt.Printf("prefix=%q limit=%d\n", prefix, limit)

	it := kvdb.NewIterator(prefixBytes, nil)
	defer it.Release()

	count := 0
	for ; count < limit && it.Next(); count++ {
		key := common.CopyBytes(it.Key())
		val := common.CopyBytes(it.Value())
		fmt.Printf("[%d] key=%x len(key)=%d type=%s len(value)=%d\n", count, key, len(key), describePebbleKey(key), len(val))
		if detail := describePebbleKeyFields(key); detail != "" {
			fmt.Printf("    keyFields=%s\n", detail)
		}
		if summary := decodePebbleValue(key, val); summary != "" {
			fmt.Printf("    decoded=%s\n", summary)
		}
		fmt.Printf("    value=%x\n", val)
	}
	if err := it.Error(); err != nil {
		fmt.Printf("⚠️ 迭代出错: %v\n", err)
		return
	}
	fmt.Printf("已输出 %d 条记录\n", count)
}

func describePebbleKey(key []byte) string {
	if len(key) == 0 {
		return "empty"
	}
	switch string(key) {
	case "DatabaseVersion":
		return "database-version"
	case "LastHeader":
		return "last-header"
	case "LastBlock":
		return "last-block"
	case "LastFast":
		return "last-fast-block"
	case "LastFinalized":
		return "last-finalized-block"
	case "SnapshotGenerator":
		return "snapshot-generator"
	case "SnapshotRoot":
		return "snapshot-root"
	case "TransactionIndexTail":
		return "tx-index-tail"
	case "unclean-shutdown":
		return "unclean-shutdown"
	}
	if strings.HasPrefix(string(key), "ethereum-config-") {
		return "chain-config"
	}
	if strings.HasPrefix(string(key), "ethereum-genesis-") {
		return "genesis-state-spec"
	}
	if strings.HasPrefix(string(key), "secure-key-") {
		return "preimage"
	}
	if strings.HasPrefix(string(key), "fm-") {
		return "filter-map"
	}
	switch key[0] {
	case 'A':
		return "path-account-trie-node"
	case 'O':
		return "path-storage-trie-node"
	case 'c':
		return "contract-code"
	case 'a':
		return "snapshot-account"
	case 'o':
		return "snapshot-storage"
	case 'h':
		return "header"
	case 'H':
		return "header-number"
	case 'b':
		return "block-body"
	case 'r':
		return "receipts"
	case 'l':
		return "tx-lookup"
	case 'B':
		return "bloom-bits"
	case 'L':
		return "state-id"
	default:
		if len(key) == common.HashLength {
			return "hash-scheme-trie-or-legacy-code"
		}
		return "unknown"
	}
}

func decodePebbleValue(key, val []byte) string {
	if len(key) == 0 || len(val) == 0 {
		return ""
	}
	switch string(key) {
	case "DatabaseVersion":
		var version uint64
		if err := rlp.DecodeBytes(val, &version); err == nil {
			return toJSON(map[string]any{"databaseVersion": version})
		}
		return toJSON(map[string]any{"databaseVersionRaw": fmt.Sprintf("%x", val)})
	case "LastHeader":
		return toJSON(map[string]any{"lastHeader": fmt.Sprintf("%x", val)})
	case "LastBlock":
		return toJSON(map[string]any{"lastBlock": fmt.Sprintf("%x", val)})
	case "LastFast":
		return toJSON(map[string]any{"lastFast": fmt.Sprintf("%x", val)})
	case "LastFinalized":
		return toJSON(map[string]any{"lastFinalized": fmt.Sprintf("%x", val)})
	case "SnapshotRoot":
		return toJSON(map[string]any{"snapshotRoot": fmt.Sprintf("%x", val)})
	case "TransactionIndexTail":
		if len(val) == 8 {
			return toJSON(map[string]any{"txIndexTail": binary.BigEndian.Uint64(val)})
		}
		return toJSON(map[string]any{"txIndexTailRaw": fmt.Sprintf("%x", val)})
	case "SnapshotGenerator":
		var fields []rlp.RawValue
		if err := rlp.DecodeBytes(val, &fields); err == nil {
			return toJSON(map[string]any{"snapshotGenerator": map[string]any{"rlpFields": len(fields)}})
		}
		return toJSON(map[string]any{"snapshotGenerator": map[string]any{"bytes": len(val)}})
	case "unclean-shutdown":
		var marker uncleanShutdownMarker
		if err := rlp.DecodeBytes(val, &marker); err == nil {
			return toJSON(marker)
		}
		return toJSON(map[string]any{"uncleanShutdownRaw": fmt.Sprintf("%x", val)})
	}
	if strings.HasPrefix(string(key), "ethereum-config-") {
		return decodeJSONValue(val)
	}
	if strings.HasPrefix(string(key), "ethereum-genesis-") {
		return decodeJSONValue(val)
	}
	if strings.HasPrefix(string(key), "secure-key-") {
		hashed := key[len("secure-key-"):]
		return toJSON(map[string]any{
			"hash":        fmt.Sprintf("%x", hashed),
			"preimageLen": len(val),
			"preimage":    fmt.Sprintf("%x", val),
		})
	}
	if strings.HasPrefix(string(key), "fm-") {
		return decodeFilterMapValue(key, val)
	}
	switch key[0] {
	case 'h':
		var header types.Header
		if err := rlp.DecodeBytes(val, &header); err == nil {
			return toJSON(&header)
		}
	case 'b':
		var body types.Body
		if err := rlp.DecodeBytes(val, &body); err == nil {
			return toJSON(&body)
		}
	case 'r':
		var receipts []*types.ReceiptForStorage
		if err := rlp.DecodeBytes(val, &receipts); err == nil {
			return toJSON(receipts)
		}
	case 'c':
		return toJSON(map[string]any{"hash": crypto.Keccak256Hash(val), "size": len(val)})
	case 'A':
		return toJSON(accountTrieNodeJSON(key[1:], val))
	case 'O':
		if len(key) >= 1+common.HashLength {
			owner := key[1 : 1+common.HashLength]
			path := key[1+common.HashLength:]
			node := accountTrieNodeJSON(path, val)
			node["owner"] = fmt.Sprintf("%x", owner)
			return toJSON(node)
		}
		return toJSON(map[string]any{"nodeHash": crypto.Keccak256Hash(val)})
	case 'H':
		if len(key) == 1+common.HashLength && len(val) == 8 {
			return toJSON(map[string]any{"hash": fmt.Sprintf("%x", key[1:]), "number": binary.BigEndian.Uint64(val)})
		}
		return toJSON(map[string]any{"headerNumberRaw": fmt.Sprintf("%x", val)})
	case 'L':
		if len(key) == 1+common.HashLength && len(val) == 8 {
			return toJSON(map[string]any{"root": fmt.Sprintf("%x", key[1:]), "id": binary.BigEndian.Uint64(val)})
		}
		return toJSON(map[string]any{"stateIDRaw": fmt.Sprintf("%x", val)})
	case 'a':
		var account types.StateAccount
		if err := rlp.DecodeBytes(val, &account); err == nil {
			return toJSON(map[string]any{
				"nonce":       account.Nonce,
				"balance":     account.Balance.String(),
				"storageRoot": account.Root,
				"codeHash":    fmt.Sprintf("%x", account.CodeHash),
			})
		}
	case 'o':
		return toJSON(map[string]any{"snapshotStorageRaw": fmt.Sprintf("%x", val)})
	default:
		if len(key) == common.HashLength {
			return toJSON(map[string]any{
				"hash":         fmt.Sprintf("%x", key),
				"keccakValue":  crypto.Keccak256Hash(val),
				"match":        common.BytesToHash(key) == crypto.Keccak256Hash(val),
			})
		}
	}
	return ""
}

func describePebbleKeyFields(key []byte) string {
	if len(key) == 0 {
		return ""
	}
	switch string(key) {
	case "DatabaseVersion":
		return "name=DatabaseVersion"
	case "LastHeader":
		return "name=LastHeader"
	case "LastBlock":
		return "name=LastBlock"
	case "LastFast":
		return "name=LastFast"
	case "LastFinalized":
		return "name=LastFinalized"
	case "SnapshotGenerator":
		return "name=SnapshotGenerator"
	case "SnapshotRoot":
		return "name=SnapshotRoot"
	case "TransactionIndexTail":
		return "name=TransactionIndexTail"
	case "unclean-shutdown":
		return "name=unclean-shutdown"
	}
	if strings.HasPrefix(string(key), "ethereum-config-") {
		return fmt.Sprintf("name=ethereum-config genesisHash=%x", key[len("ethereum-config-"):])
	}
	if strings.HasPrefix(string(key), "ethereum-genesis-") {
		return fmt.Sprintf("name=ethereum-genesis genesisHash=%x", key[len("ethereum-genesis-"):])
	}
	if strings.HasPrefix(string(key), "secure-key-") {
		return fmt.Sprintf("prefix=secure-key hash=%x", key[len("secure-key-"):])
	}
	if strings.HasPrefix(string(key), "fm-") {
		return describeFilterMapKey(key)
	}
	switch key[0] {
	case 'A':
		return fmt.Sprintf("prefix=A path=%x", key[1:])
	case 'O':
		if len(key) < 1+common.HashLength {
			return "prefix=O malformed=true"
		}
		accountHash := key[1 : 1+common.HashLength]
		path := key[1+common.HashLength:]
		return fmt.Sprintf("prefix=O accountHash=%x path=%x", accountHash, path)
	case 'H':
		if len(key) == 1+common.HashLength {
			return fmt.Sprintf("prefix=H hash=%x", key[1:])
		}
		return "prefix=H malformed=true"
	case 'L':
		if len(key) == 1+common.HashLength {
			return fmt.Sprintf("prefix=L root=%x", key[1:])
		}
		return "prefix=L malformed=true"
	default:
		return ""
	}
}

type uncleanShutdownMarker struct {
	Discarded uint64
	Recent    []uint64
}

func decodeJSONValue(val []byte) string {
	var v any
	if err := json.Unmarshal(val, &v); err != nil {
		return toJSON(map[string]any{"bytes": len(val)})
	}
	return toJSON(v)
}

func describeFilterMapKey(key []byte) string {
	if string(key) == "fm-R" {
		return "prefix=fm-R"
	}
	if strings.HasPrefix(string(key), "fm-b") && len(key) == 8 {
		return fmt.Sprintf("prefix=fm-b mapIndex=%d", binary.BigEndian.Uint32(key[4:8]))
	}
	if strings.HasPrefix(string(key), "fm-p") && len(key) == 12 {
		return fmt.Sprintf("prefix=fm-p blockNumber=%d", binary.BigEndian.Uint64(key[4:12]))
	}
	if strings.HasPrefix(string(key), "fm-r") && len(key) == 13 {
		return fmt.Sprintf("prefix=fm-r rowIndex=%d", binary.BigEndian.Uint64(key[4:12]))
	}
	return "prefix=fm unknown=true"
}

func decodeFilterMapValue(key, val []byte) string {
	switch {
	case string(key) == "fm-R":
		var fields []rlp.RawValue
		if err := rlp.DecodeBytes(val, &fields); err == nil {
			return toJSON(map[string]any{"filterMapRange": map[string]any{"rlpFields": len(fields)}})
		}
		return toJSON(map[string]any{"filterMapRange": map[string]any{"bytes": len(val)}})
	case strings.HasPrefix(string(key), "fm-b") && len(val) == 8:
		return toJSON(map[string]any{"filterMapLastBlock": binary.BigEndian.Uint64(val)})
	case strings.HasPrefix(string(key), "fm-p") && len(val) == 8:
		return toJSON(map[string]any{"filterMapBlockLV": binary.BigEndian.Uint64(val)})
	case strings.HasPrefix(string(key), "fm-r"):
		return toJSON(map[string]any{"filterMapRow": map[string]any{"bytes": len(val)}})
	default:
		return toJSON(map[string]any{"filterMapRaw": fmt.Sprintf("%x", val)})
	}
}

func toJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("{\"marshalError\":%q}", err.Error())
	}
	return string(data)
}

func accountTrieNodeJSON(path, val []byte) map[string]any {
	info := map[string]any{
		"isRoot":     len(path) == 0,
		"path":       fmt.Sprintf("%x", path),
		"pathLength": len(path),
		"nodeHash":   crypto.Keccak256Hash(val),
	}
	for k, v := range decodeTrieNodeKind(val) {
		info[k] = v
	}
	return info
}

func decodeTrieNodeKind(val []byte) map[string]any {
	elems, err := rlp.SplitListValues(val)
	if err != nil {
		return map[string]any{"nodeKind": "unknown", "decodeError": err.Error()}
	}
	switch len(elems) {
	case 2:
		keyContent, _, err := rlp.SplitString(elems[0])
		if err != nil {
			return map[string]any{"nodeKind": "short", "keyDecodeError": err.Error()}
		}
		isLeaf, nibbleLen := compactKeyInfo(keyContent)
		if isLeaf {
			return map[string]any{"nodeKind": "leaf", "keyNibbleLength": nibbleLen}
		}
		return map[string]any{"nodeKind": "short", "keyNibbleLength": nibbleLen}
	case 17:
		nonEmptyChildren := 0
		for i := 0; i < 16; i++ {
			if len(elems[i]) > 0 && elems[i][0] != 0x80 {
				nonEmptyChildren++
			}
		}
		hasValue := len(elems[16]) > 0 && elems[16][0] != 0x80
		return map[string]any{"nodeKind": "full", "children": nonEmptyChildren, "hasValue": hasValue}
	default:
		return map[string]any{"nodeKind": "unknown", "listElements": len(elems)}
	}
}

func compactKeyInfo(compact []byte) (bool, int) {
	if len(compact) == 0 {
		return false, 0
	}
	first := compact[0]
	isLeaf := (first & 0x20) != 0
	odd := (first & 0x10) != 0
	nibbles := (len(compact) - 1) * 2
	if odd {
		nibbles++
	}
	return isLeaf, nibbles
}
