package services

import (
	"crawlab/constants"
	"crawlab/database"
	"crawlab/entity"
	"crawlab/lib/cron"
	"crawlab/model"
	"crawlab/services/local_node"
	"crawlab/services/msg_handler"
	"crawlab/utils"
	"encoding/json"
	"fmt"
	"github.com/apex/log"
	"github.com/globalsign/mgo/bson"
	"github.com/gomodule/redigo/redis"
	"github.com/spf13/viper"
	"runtime/debug"
	"time"
)

type Data struct {
	Key          string    `json:"key"`
	Mac          string    `json:"mac"`
	Ip           string    `json:"ip"`
	Hostname     string    `json:"hostname"`
	Master       bool      `json:"master"`
	UpdateTs     time.Time `json:"update_ts"`
	UpdateTsUnix int64     `json:"update_ts_unix"`
}

// 所有调用IsMasterNode的方法，都永远会在master节点执行，所以GetCurrentNode方法返回永远是master节点
// 该ID的节点是否为主节点
func IsMasterNode(id string) bool {
	curNode := local_node.CurrentNode()
	//curNode, _ := model.GetCurrentNode()
	node, _ := model.GetNode(bson.ObjectIdHex(id))
	return curNode.Id == node.Id
}

// 获取节点数据
func GetNodeData() (Data, error) {
	localNode := local_node.GetLocalNode()
	key := localNode.Identify
	if key == "" {
		return Data{}, nil
	}

	value, err := database.RedisClient.HGet("nodes", key)
	data := Data{}
	if err := json.Unmarshal([]byte(value), &data); err != nil {
		return data, err
	}
	return data, err
}
func GetRedisNode(key string) (*Data, error) {
	// 获取节点数据
	value, err := database.RedisClient.HGet("nodes", key)
	if err != nil {
		log.Errorf(err.Error())
		return nil, err
	}

	// 解析节点列表数据
	var data Data
	if err := json.Unmarshal([]byte(value), &data); err != nil {
		log.Errorf(err.Error())
		return nil, err
	}
	return &data, nil
}

// 更新所有节点状态
func UpdateNodeStatus() {
	// 从Redis获取节点keys
	list, err := database.RedisClient.HScan("nodes")
	if err != nil {
		log.Errorf("get redis node keys error: %s", err.Error())
		return
	}
	var offlineKeys []string
	// 遍历节点keys
	for _, dataStr := range list {
		var data Data
		if err := json.Unmarshal([]byte(dataStr), &data); err != nil {
			log.Errorf(err.Error())
			continue
		}
		// 如果记录的更新时间超过60秒，该节点被认为离线
		if time.Now().Unix()-data.UpdateTsUnix > 60 {
			offlineKeys = append(offlineKeys, data.Key)
			// 在Redis中删除该节点
			if err := database.RedisClient.HDel("nodes", data.Key); err != nil {
				log.Errorf("delete redis node key error:%s, key:%s", err.Error(), data.Key)
			}
			continue
		}

		// 处理node信息
		if err = UpdateNodeInfo(&data); err != nil {
			log.Errorf(err.Error())
			continue
		}
	}
	if len(offlineKeys) > 0 {
		s, c := database.GetCol("nodes")
		defer s.Close()
		_, err = c.UpdateAll(bson.M{
			"key": bson.M{
				"$in": offlineKeys,
			},
		}, bson.M{
			"$set": bson.M{
				"status":         constants.StatusOffline,
				"update_ts":      time.Now(),
				"update_ts_unix": time.Now().Unix(),
			},
		})
		if err != nil {
			log.Errorf(err.Error())
		}
	}
}

func getNodeName(data *Data) string {
	registerType := viper.GetString("server.register.type")
	if registerType == constants.RegisterTypeMac {
		return data.Ip
	} else if registerType == constants.RegisterTypeIp {
		return data.Ip
	} else if registerType == constants.RegisterTypeHostname {
		return data.Hostname
	} else {
		return data.Ip
	}
}

// 处理节点信息
func UpdateNodeInfo(data *Data) (err error) {
	// 更新节点信息到数据库
	s, c := database.GetCol("nodes")
	defer s.Close()

	_, err = c.Upsert(bson.M{"key": data.Key}, bson.M{
		"$set": bson.M{
			"status":         constants.StatusOnline,
			"update_ts":      time.Now(),
			"update_ts_unix": time.Now().Unix(),
		},
		"$setOnInsert": bson.M{
			"_id":       bson.NewObjectId(),
			"key":       data.Key,
			"name":      getNodeName(data),
			"ip":        data.Ip,
			"port":      "8000",
			"mac":       data.Mac,
			"is_master": data.Master,
		},
	})
	return err
}

// 更新节点数据
func UpdateNodeData() {
	localNode := local_node.GetLocalNode()
	key := localNode.Identify
	// 构造节点数据
	data := Data{
		Key:          key,
		Mac:          localNode.Mac,
		Ip:           localNode.Ip,
		Hostname:     localNode.Hostname,
		Master:       model.IsMaster(),
		UpdateTs:     time.Now(),
		UpdateTsUnix: time.Now().Unix(),
	}

	// 注册节点到Redis
	dataBytes, err := json.Marshal(&data)
	if err != nil {
		log.Errorf(err.Error())
		debug.PrintStack()
		return
	}

	if err := database.RedisClient.HSet("nodes", key, utils.BytesToString(dataBytes)); err != nil {
		log.Errorf(err.Error())
		return
	}
}

func MasterNodeCallback(message redis.Message) (err error) {
	// 反序列化
	var msg entity.NodeMessage
	if err := json.Unmarshal(message.Data, &msg); err != nil {

		return err
	}

	if msg.Type == constants.MsgTypeGetLog {
		// 获取日志
		time.Sleep(10 * time.Millisecond)
		ch := TaskLogChanMap.ChanBlocked(msg.TaskId)
		ch <- msg.Log
	} else if msg.Type == constants.MsgTypeGetSystemInfo {
		// 获取系统信息
		fmt.Println(msg)
		time.Sleep(10 * time.Millisecond)
		ch := SystemInfoChanMap.ChanBlocked(msg.NodeId)
		sysInfoBytes, _ := json.Marshal(&msg.SysInfo)
		ch <- utils.BytesToString(sysInfoBytes)
	}
	return nil
}

func WorkerNodeCallback(message redis.Message) (err error) {
	// 反序列化
	msg := utils.GetMessage(message)
	if err := msg_handler.GetMsgHandler(*msg).Handle(); err != nil {
		log.Errorf("msg handler error: %s", err.Error())
		debug.PrintStack()
		return err
	}
	return nil
}

// 初始化节点服务
func InitNodeService() error {
	node, err := local_node.InitLocalNode()
	if err != nil {
		return err
	}

	// 构造定时任务
	c := cron.New(cron.WithSeconds())

	// 每5秒更新一次本节点信息
	spec := "0/5 * * * * *"
	if _, err := c.AddFunc(spec, UpdateNodeData); err != nil {
		debug.PrintStack()
		return err
	}

	// 首次更新节点数据（注册到Redis）
	UpdateNodeData()

	err = node.Ready()

	if err != nil {
		return err
	}

	if model.IsMaster() {
		err = model.UpdateMasterNodeInfo(node.Identify, node.Ip, node.Mac, node.Hostname)

		if err != nil {
			return err
		}
		// 如果为主节点，订阅主节点通信频道
		if err := database.Sub(constants.ChannelMasterNode, MasterNodeCallback); err != nil {
			return err
		}
	} else {
		// 若为工作节点，订阅单独指定通信频道
		channel := constants.ChannelWorkerNode + node.Current().Id.Hex()
		if err := database.Sub(channel, WorkerNodeCallback); err != nil {
			return err
		}
	}

	// 订阅全通道
	if err := database.Sub(constants.ChannelAllNode, WorkerNodeCallback); err != nil {
		return err
	}

	// 如果为主节点，每10秒刷新所有节点信息
	if model.IsMaster() {
		spec := "*/10 * * * * *"
		if _, err := c.AddFunc(spec, UpdateNodeStatus); err != nil {
			debug.PrintStack()
			return err
		}
	}

	// 更新在当前节点执行中的任务状态为：abnormal
	if err := model.UpdateTaskToAbnormal(node.Current().Id); err != nil {
		debug.PrintStack()
		return err
	}

	c.Start()
	return nil
}
