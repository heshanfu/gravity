package mysqlstream

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/juju/errors"
	gomysql "github.com/siddontang/go-mysql/mysql"
	"github.com/stretchr/testify/assert"

	"github.com/moiot/gravity/mock/binlog_checker"
	"github.com/moiot/gravity/mock/position_store"
	"github.com/moiot/gravity/pkg/core"
	"github.com/moiot/gravity/pkg/emitter"
	"github.com/moiot/gravity/pkg/mysql_test"
	"github.com/moiot/gravity/pkg/schema_store"
	"github.com/moiot/gravity/pkg/utils"
)

type fakeDB struct{}

var master = "58ff439a-c2e2-11e6-bdc7-125c95d674c1"
var slave1 = "58ff439a-c2e2-11e6-bdc7-125c95d674c2"
var slave2 = "58ff439a-c2e2-11e6-bdc7-125c95d674c3"

var masterGTID = fmt.Sprintf("%s:1-1000", master)
var slave1GTID = fmt.Sprintf("%s:1-4", slave1)
var slave2GTID = fmt.Sprintf("%s:1-45", slave2)

func (db fakeDB) GetMasterStatus() (gomysql.Position, gomysql.MysqlGTIDSet, error) {
	gtidSetString := fmt.Sprintf("%s,%s,%s", slave1GTID, masterGTID, slave2GTID)
	gtidSet, err := gomysql.ParseMysqlGTIDSet(gtidSetString)
	if err != nil {
		return gomysql.Position{}, gomysql.MysqlGTIDSet{}, errors.Trace(err)
	}

	mysqlGTIDSet := gtidSet.(*gomysql.MysqlGTIDSet)
	return gomysql.Position{Name: "test", Pos: 1}, *mysqlGTIDSet, nil
}

func (db fakeDB) GetServerUUID() (string, error) {
	return master, nil
}

func TestFixGTID(t *testing.T) {
	assert := assert.New(t)

	binlogPosition := utils.MySQLBinlogPosition{
		BinLogFileName: "test",
		BinLogFilePos:  1,
		BinlogGTID:     fmt.Sprintf("%s,%s", masterGTID, slave1GTID),
	}

	pos, err := fixGTID(fakeDB{}, binlogPosition)
	if err != nil {
		assert.FailNowf("failed to fixGTID: ", errors.ErrorStack(err))
	}

	assert.True(strings.Contains(pos.BinlogGTID, masterGTID))
	assert.True(strings.Contains(pos.BinlogGTID, slave1GTID))
	assert.True(strings.Contains(pos.BinlogGTID, slave2GTID))
}

type fakeMsgSubmitter struct {
	msgs []*core.Msg
}

func (submitter *fakeMsgSubmitter) SubmitMsg(msg *core.Msg) error {
	if msg.Type == core.MsgDML {
		submitter.msgs = append(submitter.msgs, msg)
	}
	return nil
}

func TestMsgEmit(t *testing.T) {
	assert := assert.New(t)
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	submitter := &fakeMsgSubmitter{}
	emitter, err := emitter.NewEmitter(nil, submitter)
	if err != nil {
		assert.FailNow(err.Error())
	}

	sourceDBName := "binlog_tailer_test"
	db := mysql_test.MustSetupSourceDB(sourceDBName)

	binlogFilter := func(schemaName string) bool {
		return schemaName == sourceDBName
	}

	pluginCfg := MySQLBinlogInputPluginConfig{
		Source: mysql_test.SourceDBConfig(),
	}

	// setup mock position store to return latest postiion
	dbUtils := utils.NewMySQLDB(db)
	position, gtidSet, err := dbUtils.GetMasterStatus()
	assert.Nil(err)

	positionConfig := utils.MySQLBinlogPosition{
		BinLogFileName: position.Name,
		BinLogFilePos:  position.Pos,
		BinlogGTID:     gtidSet.String(),
	}

	mockPositionStore := mock_position_store.NewMockMySQLPositionStore(mockCtrl)
	mockPositionStore.EXPECT().Get().Return(positionConfig).AnyTimes()

	// mockBinlogChecker
	mockBinlogChecker := mock_binlog_checker.NewMockBinlogChecker(mockCtrl)
	mockBinlogChecker.EXPECT().IsEventBelongsToMySelf(gomock.Any()).AnyTimes().Return(false)

	schemaStore, err := schema_store.NewSimpleSchemaStoreFromDBConn(db)

	binlogTailer, err := NewBinlogTailer(
		t.Name(),
		&pluginCfg,
		100,
		mockPositionStore,
		schemaStore,
		db,
		emitter,
		mockBinlogChecker,
		binlogFilter)
	if err != nil {
		assert.FailNow(err.Error())
	}

	err = binlogTailer.Start()
	if err != nil {
		assert.FailNow(err.Error())
	}

	// 1.
	err = mysql_test.InsertIntoTestTable(db,
		sourceDBName,
		mysql_test.TestTableName,
		map[string]interface{}{
			"id":   1,
			"name": "test1",
			"ts":   time.Now(),
		})
	assert.Nil(err)

	// 2.
	err = mysql_test.InsertIntoTestTable(
		db,
		sourceDBName,
		mysql_test.TestTableName,
		map[string]interface{}{
			"id":   2,
			"name": "test2",
			"ts":   time.Now(),
		})
	assert.Nil(err)

	// 3.
	err = mysql_test.InsertIntoTestTable(db, sourceDBName, mysql_test.TestTableName,
		map[string]interface{}{
			"id":   3,
			"name": "test3",
			"ts":   time.Now(),
		})
	assert.Nil(err)

	// 4. update the first row
	err = mysql_test.UpdateTestTable(db, sourceDBName, mysql_test.TestTableName, 1, "test11")
	assert.Nil(err)

	// 5
	err = mysql_test.UpdateTestTable(db, sourceDBName, mysql_test.TestTableName, 2, "test22")
	assert.Nil(err)

	err = mysql_test.SendDeadSignal(db, 100)
	assert.Nil(err)

	select {
	case <-binlogTailer.done:
	case <-time.After(20 * time.Second):
		assert.FailNow("timeout")
	}

	// check received core.Msg
	assert.Equal(5, len(submitter.msgs))
}
