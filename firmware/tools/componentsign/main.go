// Command componentsign 生成并使用 LLM Gate官网组件清单的离线签名密钥。
//
// 固件只信任内嵌的 Ed25519 公钥（internal/cloudflared/keys.go）；官网发布的
// /updates/components/<组件>/stable.json 必须附带用对应私钥签出的 stable.json.sig，
// 设备验签、检查 revision 防回退后才把清单当作允许决策。
//
// 私钥文件是单行 base64 的 64 字节 Ed25519 私钥（crypto/ed25519.PrivateKey），
// 只存在于项目根 gitignored 的 .component-signing-key，绝不入库、不进 CI、不进日志。
//
// 用法：
//
//	componentsign keygen -out <私钥文件>            生成密钥对，stdout 打印公钥 base64
//	componentsign pubkey -key <私钥文件>            打印公钥 base64
//	componentsign sign -key <私钥文件> -key-id <id> -in stable.json -out stable.json.sig
//	componentsign verify -pub <公钥 base64> -in stable.json -sig stable.json.sig
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

// signatureSchema 与 internal/cloudflared 的 SignatureSchema 同值。
const signatureSchema = "llmgate.component-signature/v1"

type signatureFile struct {
	Schema    string `json:"schema"`
	KeyID     string `json:"keyId"`
	Algorithm string `json:"algorithm"`
	Signature string `json:"signature"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "pubkey":
		err = pubkey(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "componentsign:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `用法:
  componentsign keygen -out <私钥文件>
  componentsign pubkey -key <私钥文件>
  componentsign sign -key <私钥文件> -key-id <id> -in <stable.json> -out <stable.json.sig>
  componentsign verify -pub <公钥 base64> -in <stable.json> -sig <stable.json.sig>
`)
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("out", "", "私钥文件路径（0600，已存在则拒绝覆盖）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("-out 必填")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("创建私钥文件: %w", err)
	}
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(priv) + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Println(base64.StdEncoding.EncodeToString(pub))
	return nil
}

func loadPriv(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取私钥: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("私钥文件不是单行 base64 的 Ed25519 私钥")
	}
	return ed25519.PrivateKey(key), nil
}

func pubkey(args []string) error {
	fs := flag.NewFlagSet("pubkey", flag.ContinueOnError)
	key := fs.String("key", "", "私钥文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	priv, err := loadPriv(*key)
	if err != nil {
		return err
	}
	fmt.Println(base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)))
	return nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	key := fs.String("key", "", "私钥文件")
	keyID := fs.String("key-id", "", "公钥标识（固件内嵌表的键）")
	in := fs.String("in", "", "待签清单 stable.json")
	out := fs.String("out", "", "签名输出 stable.json.sig")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *key == "" || *keyID == "" || *in == "" || *out == "" {
		return errors.New("-key、-key-id、-in、-out 都必填")
	}
	priv, err := loadPriv(*key)
	if err != nil {
		return err
	}
	msg, err := os.ReadFile(*in)
	if err != nil {
		return fmt.Errorf("读取清单: %w", err)
	}
	if !json.Valid(msg) {
		return errors.New("清单不是合法 JSON")
	}
	sig := ed25519.Sign(priv, msg)
	raw, _ := json.MarshalIndent(signatureFile{
		Schema: signatureSchema, KeyID: *keyID, Algorithm: "ed25519",
		Signature: base64.StdEncoding.EncodeToString(sig),
	}, "", "  ")
	return os.WriteFile(*out, append(raw, '\n'), 0o644)
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	pub := fs.String("pub", "", "公钥 base64")
	in := fs.String("in", "", "清单 stable.json")
	sigPath := fs.String("sig", "", "签名 stable.json.sig")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pubKey, err := base64.StdEncoding.DecodeString(strings.TrimSpace(*pub))
	if err != nil || len(pubKey) != ed25519.PublicKeySize {
		return errors.New("公钥不是 base64 的 Ed25519 公钥")
	}
	msg, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	rawSig, err := os.ReadFile(*sigPath)
	if err != nil {
		return err
	}
	var sf signatureFile
	if err := json.Unmarshal(rawSig, &sf); err != nil || sf.Schema != signatureSchema || sf.Algorithm != "ed25519" {
		return errors.New("签名文件形态不符")
	}
	sig, err := base64.StdEncoding.DecodeString(sf.Signature)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(pubKey), msg, sig) {
		return errors.New("签名校验失败")
	}
	fmt.Println("OK", sf.KeyID)
	return nil
}
