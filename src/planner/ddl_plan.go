/*
 * Radon
 *
 * Copyright 2018 The Radon Authors.
 * Code is licensed under the GPLv3.
 *
 */

package planner

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"router"
	"xcontext"

	"github.com/xelabs/go-mysqlstack/sqlparser"
	"github.com/xelabs/go-mysqlstack/sqlparser/depends/common"
	"github.com/xelabs/go-mysqlstack/xlog"
)

var (
	_ Plan = &DDLPlan{}
)

// DDLPlan represents a CREATE, ALTER, DROP or RENAME plan
type DDLPlan struct {
	log *xlog.Log

	// router
	router *router.Router

	// ddl ast
	node *sqlparser.DDL

	// database
	database string

	// raw query
	RawQuery string

	// type
	typ PlanType

	// mode
	ReqMode xcontext.RequestMode

	// query and backend tuple
	Querys []xcontext.QueryTuple
}

// NewDDLPlan used to create DDLPlan
func NewDDLPlan(log *xlog.Log, database string, query string, node *sqlparser.DDL, router *router.Router) *DDLPlan {
	return &DDLPlan{
		log:      log,
		node:     node,
		router:   router,
		database: database,
		RawQuery: query,
		typ:      PlanTypeDDL,
		Querys:   make([]xcontext.QueryTuple, 0, 16),
	}
}

// Build used to build DDL distributed querys.
// sqlparser.DDL is a simple grammar ast, it just parses database and table name in the prefix.
func (p *DDLPlan) Build() error {
	node := p.node

	switch node.Action {
	case sqlparser.CreateDBStr:
		p.ReqMode = xcontext.ReqScatter
		return nil
	default:
		table := node.Table.Name.String()
		database := p.database
		if !node.Table.Qualifier.IsEmpty() {
			database = node.Table.Qualifier.String()
		}

		// Get the shard key.
		shardKey, err := p.router.ShardKey(database, table)
		if err != nil {
			return err
		}
		// Unsupported operations check if shardtype is HASH.
		if shardKey != "" {
			switch node.Action {
			case sqlparser.AlterDropColumnStr:
				if shardKey == node.DropColumnName {
					return errors.New("unsupported: cannot.drop.the.column.on.shard.key")
				}
			case sqlparser.AlterModifyColumnStr:
				if shardKey == node.ModifyColumnDef.Name.String() {
					return errors.New("unsupported: cannot.modify.the.column.on.shard.key")
				}
				// constraint check in column definition
				switch node.ModifyColumnDef.Type.KeyOpt {
				case sqlparser.ColKeyUnique, sqlparser.ColKeyUniqueKey, sqlparser.ColKeyPrimary, sqlparser.ColKey:
					err := fmt.Sprintf("The unique/primary constraint should be only defined on the sharding key column[%s]", shardKey)
					return errors.New(err)
				}
			case sqlparser.AlterAddColumnStr:
				//constraint check in column definition
				for _, col := range node.TableSpec.Columns {
					switch col.Type.KeyOpt {
					case sqlparser.ColKeyUnique, sqlparser.ColKeyUniqueKey, sqlparser.ColKeyPrimary, sqlparser.ColKey:
						err := fmt.Sprintf("The unique/primary constraint should be only defined on the sharding key column[%s]", shardKey)
						return errors.New(err)
					}
				}
				// constraint check in index definition
				for _, index := range node.TableSpec.Indexes {
					info := index.Info
					if info.Unique || info.Primary {
						err := fmt.Sprintf("The unique/primary constraint should be only defined on the sharding key column[%s]", shardKey)
						return errors.New(err)
					}
				}
			}
		}

		segments, err := p.router.Lookup(database, table, nil, nil)
		if err != nil {
			return err
		}
		for _, segment := range segments {
			var query string

			segTable := segment.Table
			if node.Table.Qualifier.IsEmpty() {
				segTable = fmt.Sprintf("`%s`.`%s`", database, segTable)
				rawQuery := strings.Replace(p.RawQuery, "`", "", 2)
				// \b: https://www.regular-expressions.info/wordboundaries.html
				re, _ := regexp.Compile(fmt.Sprintf(`\b(%s)\b`, table))
				query = re.ReplaceAllString(rawQuery, segTable)
			} else {
				segTable = fmt.Sprintf("`%s`.`%s`", database, segTable)
				newTable := fmt.Sprintf("%s.%s", database, table)
				rawQuery := strings.Replace(p.RawQuery, "`", "", 4)
				re, _ := regexp.Compile(fmt.Sprintf(`\b(%s)\b`, newTable))
				query = re.ReplaceAllString(rawQuery, segTable)
			}

			tuple := xcontext.QueryTuple{
				Query:   query,
				Backend: segment.Backend,
				Range:   segment.Range.String(),
			}
			p.Querys = append(p.Querys, tuple)
		}
	}
	return nil
}

// Type returns the type of the plan.
func (p *DDLPlan) Type() PlanType {
	return p.typ
}

// JSON returns the plan info.
func (p *DDLPlan) JSON() string {
	type explain struct {
		RawQuery   string                `json:",omitempty"`
		Partitions []xcontext.QueryTuple `json:",omitempty"`
	}

	// Partitions.
	var parts []xcontext.QueryTuple
	parts = append(parts, p.Querys...)
	exp := &explain{
		RawQuery:   p.RawQuery,
		Partitions: parts,
	}
	bout, err := json.MarshalIndent(exp, "", "\t")
	if err != nil {
		return err.Error()
	}
	return common.BytesToString(bout)
}

// Children returns the children of the plan.
func (p *DDLPlan) Children() *PlanTree {
	return nil
}

// Size returns the memory size.
func (p *DDLPlan) Size() int {
	size := len(p.RawQuery)
	for _, q := range p.Querys {
		size += len(q.Query)
	}
	return size
}
