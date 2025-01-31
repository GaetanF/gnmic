package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/karimra/gnmic/types"
	"github.com/karimra/gnmic/utils"
	"github.com/openconfig/gnmi/proto/gnmi"
	"github.com/spf13/cobra"
)

func (c *Config) CreateDiffSubscribeRequest(cmd *cobra.Command) (*gnmi.SubscribeRequest, error) {
	sc := &types.SubscriptionConfig{
		Name:     "diff-sub",
		Models:   c.DiffModel,
		Prefix:   c.DiffPrefix,
		Target:   c.DiffTarget,
		Paths:    c.DiffPath,
		Mode:     "ONCE",
		Encoding: c.Encoding,
	}
	if flagIsSet(cmd, "qos") {
		sc.Qos = &c.DiffQos
	}
	return sc.CreateSubscribeRequest("")
}

func (c *Config) CreateDiffGetRequest() (*gnmi.GetRequest, error) {
	if c == nil {
		return nil, errors.New("invalid configuration")
	}
	encodingVal, ok := gnmi.Encoding_value[strings.Replace(strings.ToUpper(c.Encoding), "-", "_", -1)]
	if !ok {
		return nil, fmt.Errorf("invalid encoding type '%s'", c.Encoding)
	}
	req := &gnmi.GetRequest{
		UseModels: make([]*gnmi.ModelData, 0),
		Path:      make([]*gnmi.Path, 0, len(c.LocalFlags.DiffPath)),
		Encoding:  gnmi.Encoding(encodingVal),
	}
	if c.LocalFlags.GetPrefix != "" {
		gnmiPrefix, err := utils.ParsePath(c.LocalFlags.DiffPrefix)
		if err != nil {
			return nil, fmt.Errorf("prefix parse error: %v", err)
		}
		req.Prefix = gnmiPrefix
	}
	if c.LocalFlags.DiffTarget != "" {
		if req.Prefix == nil {
			req.Prefix = &gnmi.Path{}
		}
		req.Prefix.Target = c.LocalFlags.DiffTarget
	}
	if c.LocalFlags.DiffType != "" {
		dti, ok := gnmi.GetRequest_DataType_value[strings.ToUpper(c.DiffType)]
		if !ok {
			return nil, fmt.Errorf("unknown data type %s", c.DiffType)
		}
		req.Type = gnmi.GetRequest_DataType(dti)
	}
	for _, p := range c.DiffPath {
		gnmiPath, err := utils.ParsePath(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("path parse error: %v", err)
		}
		req.Path = append(req.Path, gnmiPath)
	}
	return req, nil
}
