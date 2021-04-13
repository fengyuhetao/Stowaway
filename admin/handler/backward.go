package handler

import (
	"Stowaway/admin/manager"
	"Stowaway/admin/topology"
	"Stowaway/protocol"
	"fmt"
	"net"
)

type Backward struct {
	LPort string
	RPort string
}

func NewBackward(lPort, rPort string) *Backward {
	backward := new(Backward)
	backward.LPort = lPort
	backward.RPort = rPort
	return backward
}

func (backward *Backward) LetBackward(component *protocol.MessageComponent, mgr *manager.Manager, route string, uuid string) error {
	sMessage := protocol.PrepareAndDecideWhichSProtoToLower(component.Conn, component.Secret, component.UUID)
	// test if node can listen on assigned port
	header := &protocol.Header{
		Sender:      protocol.ADMIN_UUID,
		Accepter:    uuid,
		MessageType: protocol.BACKWARDTEST,
		RouteLen:    uint32(len([]byte(route))),
		Route:       route,
	}

	testMess := &protocol.BackwardTest{
		LPortLen: uint16(len([]byte(backward.LPort))),
		LPort:    backward.LPort,
		RPortLen: uint16(len([]byte(backward.RPort))),
		RPort:    backward.RPort,
	}

	protocol.ConstructMessage(sMessage, header, testMess)
	sMessage.SendMessage()
	// node can listen on assigned port?
	if ready := <-mgr.BackwardManager.BackwardReady; !ready {
		// can't listen
		err := fmt.Errorf("Fail to map remote port %s to local port %s,node cannot listen on port %s", backward.RPort, backward.LPort, backward.RPort)
		return err
	}
	// node can listen,it means no backward service is running on the assigned port,so just register a brand new backward
	backwardTask := &manager.BackwardTask{
		Mode:  manager.B_NEWBACKWARD,
		LPort: backward.LPort,
		RPort: backward.RPort,
		UUID:  uuid,
	}
	mgr.BackwardManager.TaskChan <- backwardTask
	<-mgr.BackwardManager.ResultChan
	// tell upstream all good,just go ahead
	return nil
}

func (backward *Backward) start(mgr *manager.Manager, topo *topology.Topology, conn net.Conn, secret string, uuid string) {
	sMessage := protocol.PrepareAndDecideWhichSProtoToLower(conn, secret, protocol.ADMIN_UUID)
	// first , admin need to know the route to target node,so ask topo for the answer
	topoTask := &topology.TopoTask{
		Mode: topology.GETROUTE,
		UUID: uuid,
	}
	topo.TaskChan <- topoTask
	topoResult := <-topo.ResultChan
	route := topoResult.Route
	// ask backward manager to assign a new seq num
	backwardTask := &manager.BackwardTask{
		Mode:  manager.B_GETNEWSEQ,
		RPort: backward.RPort,
		UUID:  uuid,
	}
	mgr.BackwardManager.TaskChan <- backwardTask
	result := <-mgr.BackwardManager.ResultChan
	seq := result.BackwardSeq
	// then,we connect to target port and get the conn && tell node result
	header := &protocol.Header{
		Sender:      protocol.ADMIN_UUID,
		Accepter:    uuid,
		MessageType: protocol.BACKWARDSEQ,
		RouteLen:    uint32(len([]byte(route))),
		Route:       route,
	}

	seqSuccMess := &protocol.BackwardSeq{
		OK:       1,
		Seq:      seq,
		RPortLen: uint16(len([]byte(backward.RPort))),
		RPort:    backward.RPort,
	}

	seqFailMess := &protocol.BackwardSeq{
		OK:       0,
		Seq:      seq,
		RPortLen: uint16(len([]byte(backward.RPort))),
		RPort:    backward.RPort,
	}
	// if can't connect,tell node to stop
	backwardConn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", backward.LPort))
	if err != nil {
		protocol.ConstructMessage(sMessage, header, seqFailMess)
		sMessage.SendMessage()
		return
	}
	// if stopbackward before,then addconn will fail,that's the only reason
	// mention, at this time ,agent will not send fin mess cuz it's blocked at getting seq
	backwardTask = &manager.BackwardTask{
		Mode:  manager.B_ADDCONN,
		RPort: backward.RPort,
		UUID:  uuid,
		Seq:   seq,
		Conn:  backwardConn,
	}
	mgr.BackwardManager.TaskChan <- backwardTask
	result = <-mgr.BackwardManager.ResultChan
	if !result.OK {
		return
	}
	// if stopbackward before,then getdatachan will fail,that's the only reason
	// get datachan immediately
	backwardTask = &manager.BackwardTask{
		Mode:  manager.B_GETDATACHAN,
		RPort: backward.RPort,
		UUID:  uuid,
		Seq:   seq,
	}
	mgr.BackwardManager.TaskChan <- backwardTask
	result = <-mgr.BackwardManager.ResultChan
	if !result.OK {
		return
	}
	// get the datachan
	dataChan := result.DataChan
	// tell node it can go ahead
	protocol.ConstructMessage(sMessage, header, seqSuccMess)
	sMessage.SendMessage()
	// send fin after all done
	defer func() {
		finHeader := &protocol.Header{
			Sender:      protocol.ADMIN_UUID,
			Accepter:    uuid,
			MessageType: protocol.BACKWARDFIN,
			RouteLen:    uint32(len([]byte(route))),
			Route:       route,
		}

		finMess := &protocol.BackWardFin{
			Seq: seq,
		}

		protocol.ConstructMessage(sMessage, finHeader, finMess)
		sMessage.SendMessage()
	}()
	// proxy C2S
	go func() {
		for {
			if data, ok := <-dataChan; ok {
				backwardConn.Write(data)
			} else {
				return
			}
		}
	}()
	// proxy S2C
	dataHeader := &protocol.Header{
		Sender:      protocol.ADMIN_UUID,
		Accepter:    uuid,
		MessageType: protocol.BACKWARDDATA,
		RouteLen:    uint32(len([]byte(route))),
		Route:       route,
	}

	buffer := make([]byte, 20480)

	for {
		length, err := backwardConn.Read(buffer)
		if err != nil {
			backwardConn.Close()
			return
		}

		backwardDataMess := &protocol.BackwardData{
			Seq:     seq,
			DataLen: uint64(length),
			Data:    buffer[:length],
		}

		protocol.ConstructMessage(sMessage, dataHeader, backwardDataMess)
		sMessage.SendMessage()
	}
}

func DispatchBackwardMess(mgr *manager.Manager, topo *topology.Topology, conn net.Conn, secret string) {
	for {
		message := <-mgr.BackwardManager.BackwardMessChan

		switch message.(type) {
		case *protocol.BackwardReady:
			mess := message.(*protocol.BackwardReady)
			if mess.OK == 1 {
				mgr.BackwardManager.BackwardReady <- true
			} else {
				mgr.BackwardManager.BackwardReady <- false
			}
		case *protocol.BackwardStart:
			// get the start message from node,so just start a backward
			mess := message.(*protocol.BackwardStart)
			backward := NewBackward(mess.LPort, mess.RPort)
			go backward.start(mgr, topo, conn, secret, mess.UUID)
		case *protocol.BackwardData:
			// get node's data,just put it in the corresponding chan
			mess := message.(*protocol.BackwardData)
			mgrTask := &manager.BackwardTask{
				Mode: manager.B_GETDATACHAN_WITHOUTUUID,
				Seq:  mess.Seq,
			}
			mgr.BackwardManager.TaskChan <- mgrTask
			result := <-mgr.BackwardManager.ResultChan
			if result.OK {
				result.DataChan <- mess.Data
			}
			mgr.BackwardManager.Done <- true
		case *protocol.BackWardFin:
			mess := message.(*protocol.BackWardFin)
			mgrTask := &manager.BackwardTask{
				Mode: manager.B_CLOSETCP,
				Seq:  mess.Seq,
			}
			mgr.BackwardManager.TaskChan <- mgrTask
		}
	}
}
