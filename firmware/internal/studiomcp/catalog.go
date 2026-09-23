package studiomcp

import (
	"bytes"
	_ "embed"
	"encoding/json"

	"github.com/llm-net/llm-gate/firmware/internal/mcpserve"
)

const (
	ToolListFiles     = "list_files"
	ToolViewImage     = "view_image"
	ToolReadText      = "read_text"
	ToolWriteText     = "write_text"
	ToolGenerateImage = "generate_image"
	ToolGenerateVideo = "generate_video"
	ToolDeleteFile    = "delete_file"
	ToolRenameFile    = "rename_file"
)

// ModelDescriptions 由装配方从媒体生成能力表生成，不在工具层维护模型知识。
type ModelDescriptions struct {
	Image string
	Video string
}

//go:embed tools.json
var toolsJSON []byte

func newCatalog(descriptions ModelDescriptions) mcpserve.Catalog {
	c := mcpserve.MustCatalog(toolsJSON)
	for i, t := range c.Tools {
		var kind, description string
		switch t.Name {
		case ToolGenerateImage:
			kind, description = "image", descriptions.Image
		case ToolGenerateVideo:
			kind, description = "video", descriptions.Video
		default:
			continue
		}
		text := "生成参数，键随模型与操作而定，缺省即不带、由平台取缺省值。各模型的输入与参数：\n" + description
		quoted, err := json.Marshal(text)
		if err != nil {
			panic(err)
		}
		placeholder := []byte(`"{{params:` + kind + `}}"`)
		if !bytes.Contains(t.InputSchema, placeholder) {
			panic("studiomcp: tools.json 的 " + t.Name + " 缺少 params 占位")
		}
		c.Tools[i].InputSchema = bytes.Replace(t.InputSchema, placeholder, quoted, 1)
	}
	return c
}
