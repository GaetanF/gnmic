package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig"
	"github.com/karimra/gnmic/utils"
	"github.com/openconfig/gnmi/proto/gnmi"
	"gopkg.in/yaml.v2"
)

const (
	varFileSuffix = "_vars"
)

type UpdateItem struct {
	Path     string      `json:"path,omitempty" yaml:"path,omitempty"`
	Value    interface{} `json:"value,omitempty" yaml:"value,omitempty"`
	Encoding string      `json:"encoding,omitempty" yaml:"encoding,omitempty"`
}

type SetRequestFile struct {
	Updates  []*UpdateItem `json:"updates,omitempty" yaml:"updates,omitempty"`
	Replaces []*UpdateItem `json:"replaces,omitempty" yaml:"replaces,omitempty"`
	Deletes  []string      `json:"deletes,omitempty" yaml:"deletes,omitempty"`
}

func (c *Config) ReadSetRequestTemplate() error {
	if c.SetRequestFile == "" {
		return nil
	}
	b, err := ioutil.ReadFile(c.SetRequestFile)
	if err != nil {
		return err
	}
	if c.Debug {
		c.logger.Printf("set request file content: %s", string(b))
	}
	// read template
	c.setRequestTemplate, err = template.New("set-request").Funcs(sprig.TxtFuncMap()).Parse(string(b))
	if err != nil {
		return err
	}
	return c.readTemplateVarsFile()
}

func (c *Config) readTemplateVarsFile() error {
	if c.SetRequestVars == "" {
		ext := filepath.Ext(c.SetRequestFile)
		c.SetRequestVars = fmt.Sprintf("%s%s%s", c.SetRequestFile[0:len(c.SetRequestFile)-len(ext)], varFileSuffix, ext)
		c.logger.Printf("trying to find variable file %q", c.SetRequestVars)
		_, err := os.Stat(c.SetRequestVars)
		if os.IsNotExist(err) {
			c.SetRequestVars = ""
			return nil
		} else if err != nil {
			return err
		}
	}
	b, err := readFile(c.SetRequestVars)
	if err != nil {
		return err
	}
	if c.setRequestVars == nil {
		c.setRequestVars = make(map[string]interface{})
	}
	err = yaml.Unmarshal(b, &c.setRequestVars)
	if err != nil {
		return err
	}
	tempInterface := convert(c.setRequestVars)
	switch t := tempInterface.(type) {
	case map[string]interface{}:
		c.setRequestVars = t
	default:
		return errors.New("unexpected variables file format")
	}
	if c.Debug {
		c.logger.Printf("request vars content: %v", c.setRequestVars)
	}
	return nil
}

func (c *Config) CreateSetRequestFromFile(targetName string) (*gnmi.SetRequest, error) {
	if c.setRequestTemplate == nil {
		return nil, errors.New("missing set request template")
	}
	buf := new(bytes.Buffer)
	err := c.setRequestTemplate.Execute(buf, templateInput{
		TargetName: targetName,
		Vars:       c.setRequestVars,
	})
	if err != nil {
		return nil, err
	}
	if c.Debug {
		c.logger.Printf("target %q template result:\n%s", targetName, buf.String())
	}
	//
	reqFile := new(SetRequestFile)
	err = yaml.Unmarshal(buf.Bytes(), reqFile)
	if err != nil {
		return nil, err
	}
	sReq := &gnmi.SetRequest{
		Delete:  make([]*gnmi.Path, 0, len(reqFile.Deletes)),
		Replace: make([]*gnmi.Update, 0, len(reqFile.Replaces)),
		Update:  make([]*gnmi.Update, 0, len(reqFile.Updates)),
	}
	buf.Reset()
	for _, upd := range reqFile.Updates {
		if upd.Path == "" {
			upd.Path = "/"
		}
		gnmiPath, err := utils.ParsePath(strings.TrimSpace(upd.Path))
		if err != nil {
			return nil, err
		}

		enc := upd.Encoding
		if enc == "" {
			enc = c.GlobalFlags.Encoding
		}
		value := new(gnmi.TypedValue)
		buf.Reset()
		err = json.NewEncoder(buf).Encode(convert(upd.Value))
		if err != nil {
			return nil, err
		}
		err = setValue(value, strings.ToLower(enc), strings.TrimSpace(buf.String()))
		if err != nil {
			return nil, err
		}
		sReq.Update = append(sReq.Update, &gnmi.Update{
			Path: gnmiPath,
			Val:  value,
		})
	}
	for _, upd := range reqFile.Replaces {
		if upd.Path == "" {
			upd.Path = "/"
		}
		gnmiPath, err := utils.ParsePath(strings.TrimSpace(upd.Path))
		if err != nil {
			return nil, err
		}

		enc := upd.Encoding
		if enc == "" {
			enc = c.GlobalFlags.Encoding
		}
		value := new(gnmi.TypedValue)
		buf.Reset()
		err = json.NewEncoder(buf).Encode(convert(upd.Value))
		if err != nil {
			return nil, err
		}
		err = setValue(value, strings.ToLower(enc), strings.TrimSpace(buf.String()))
		if err != nil {
			return nil, err
		}
		sReq.Replace = append(sReq.Replace, &gnmi.Update{
			Path: gnmiPath,
			Val:  value,
		})
	}
	for _, s := range reqFile.Deletes {
		gnmiPath, err := utils.ParsePath(strings.TrimSpace(s))
		if err != nil {
			return nil, err
		}
		sReq.Delete = append(sReq.Delete, gnmiPath)
	}
	return sReq, nil
}

type templateInput struct {
	TargetName string
	Vars       map[string]interface{}
}
