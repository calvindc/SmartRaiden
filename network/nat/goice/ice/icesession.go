package ice

import (
	"fmt"

	"errors"

	"sort"

	"time"

	"net"

	"sync"

	"github.com/kataras/iris/utils"
	"github.com/SmartMeshFoundation/SmartRaiden/network/nat/goice/ice/attr"
	"github.com/nkbai/log"
	"github.com/SmartMeshFoundation/SmartRaiden/network/nat/goice/stun"
	"github.com/SmartMeshFoundation/SmartRaiden/network/nat/goice/turn"
)

type SessionRole int

const (
	SessionRoleUnkown SessionRole = iota
	SessionRoleControlling
	SessionRoleControlled
)

func (s SessionRole) String() string {
	switch s {
	case SessionRoleUnkown:
		return "unkown"
	case SessionRoleControlled:
		return "controlled"
	case SessionRoleControlling:
		return "controlling"
	}
	return "unknown"
}

type IceSession struct {
	Name string //for debug
	/*
		controlling or controlled
	*/
	role SessionRole
	/**
	 * Specify whether to use aggressive nomination.
	 */
	aggresive bool

	/**
	 * For controlling agent if it uses regular nomination, specify the delay
	 * to perform nominated check (connectivity check with USE-CANDIDATE
	 * attribute) after all components have a valid pair.
	 *
	 * Default value is PJ_ICE_NOMINATED_CHECK_DELAY.
	 */
	//nominated_check_delay int

	/**
	 * For a controlled agent, specify how long it wants to wait (in
	 * milliseconds) for the controlling agent to complete sending
	 * connectivity check with nominated flag set to true for all components
	 * after the controlled agent has found that all connectivity checks in
	 * its checklist have been completed and there is at least one successful
	 * (but not nominated) check for every component.
	 *
	 * Default value for this option is
	 * ICE_CONTROLLED_AGENT_WAIT_NOMINATION_TIMEOUT. Specify -1 to disable
	 * this timer.
	 */
	controlled_agent_want_nom_timeout int
	/* STUN credentials */
	txUserFrag       string /**< Remote ufrag.	    */
	txUserName       string /**< Uname for TX.	TxUserFrag:RxUserFrag    */
	txPassword       string /**< Remote password.   */
	rxUserFrag       string /**< Local ufrag.	    */
	rxUserName       string /**< Uname for RX	    */
	rxPassword       string /**< Local password.    */
	txCrendientials  stun.MessageIntegrity
	rxCrendientials  stun.MessageIntegrity
	sessionComponent SessionComponet
	localCandidates  []*Candidate
	remoteCandidates []*Candidate
	checkList        *SessionCheckList
	validCheckList   *SessionCheckList // check has been verified and is valid.
	transporter      StunTranporter
	serverSocks      map[string]ServerSocker
	turnServerSock   *TurnServerSock

	isNominating bool /* Nominating stage   */
	//write this chan to finish one check.
	checkMap map[string]chan error
	//todo refer state, etc.
	iceStreamTransport *IceStreamTransport
	checkResponse      chan *checkResponse
	/*
	   用于角色冲突的时候,自行进行角色切换 ICEROLECONTROLLING <--->ICEROLECONTROLLED
	*/
	tieBreaker     uint64
	earlyCheckList []*RxCheck
	msg2Check      map[stun.TransactionID]*SessionCheck
	mlock          sync.Mutex

	/*
		收到的stun message, 不要堵塞发送接收routine
	*/
	msgChan    chan *stunMessageWrapper
	dataChan   chan *stunDataWrapper
	hasStopped bool //停止销毁相关资源时,标记.
}

type checkResponse struct {
	res   *stun.Message
	from  string //this message comes from which address
	check *SessionCheck
}
type SessionComponet struct {
	/**
	 * Pointer to ICE check with highest priority which connectivity check
	 * has been successful. The value will be NULL if a no successful check
	 * has not been found for this component.
	 */
	validCheck *SessionCheck
	/**
	 * Pointer to ICE check with highest priority which connectivity check
	 * has been successful and it has been nominated. The value may be NULL
	 * if there is no such check yet.
	 */
	nominatedCheck *SessionCheck
	/*
		nominated server socker
		to send data to peer.
	*/
	nominatedServerSock ServerSocker
}

func NewIceSession(name string, role SessionRole, localCandidates []*Candidate, transporter StunTranporter, ice *IceStreamTransport) *IceSession {
	s := &IceSession{
		Name:               name,
		role:               role,
		aggresive:          true,
		rxUserFrag:         utils.RandomString(8),
		rxPassword:         utils.RandomString(8),
		localCandidates:    localCandidates,
		transporter:        transporter,
		checkMap:           make(map[string]chan error),
		iceStreamTransport: ice,
		checkResponse:      make(chan *checkResponse, 10),
		checkList:          new(SessionCheckList),
		validCheckList:     new(SessionCheckList),
		tieBreaker:         attr.RandUint64(),
		serverSocks:        make(map[string]ServerSocker),
		msg2Check:          make(map[stun.TransactionID]*SessionCheck),
		msgChan:            make(chan *stunMessageWrapper, 10),
		dataChan:           make(chan *stunDataWrapper, 10),
	}
	s.rxCrendientials = stun.NewShortTermIntegrity(s.rxPassword)
	//make sure the first candidates is used to communicate with stun/turn server

	return s
}

var errTooManyCandidates = errors.New("too many candidates")

func (s *IceSession) addMsgCheck(id stun.TransactionID, check *SessionCheck) {
	s.mlock.Lock()
	s.msg2Check[id] = check
	s.mlock.Unlock()
}
func (s *IceSession) getMsgCheck(id stun.TransactionID) *SessionCheck {
	s.mlock.Lock()
	defer s.mlock.Unlock()
	return s.msg2Check[id]
}
func (s *IceSession) Stop() {
	s.hasStopped = true
	for _, srv := range s.serverSocks {
		srv.Close()
	}
	for _, c := range s.checkMap {
		close(c)
	}
	close(s.checkResponse)
}
func (s *IceSession) createCheckList(sd *sessionDescription) error {
	if len(sd.candidates) > MaxCandidates {
		return errTooManyCandidates
	}
	s.txUserName = fmt.Sprintf("%s:%s", sd.user, s.rxUserFrag)
	s.rxUserName = fmt.Sprintf("%s:%s", s.rxUserFrag, sd.user)
	s.txPassword = sd.password
	s.txCrendientials = stun.NewShortTermIntegrity(s.txPassword)
	for _, c := range sd.candidates {
		if c.ComponentID != 1 { //only one component,
			continue
		}
		s.remoteCandidates = append(s.remoteCandidates, c)
	}
	for _, l := range s.localCandidates {
		for _, r := range s.remoteCandidates {
			chk := &SessionCheck{
				localCandidate:  l,
				remoteCandidate: r,
				key:             fmt.Sprintf("%s-%s", l.addr, r.addr),
				state:           CheckStateFrozen,
				priority:        calcPairPriority(s.role, l, r),
			}
			s.checkList.checks = append(s.checkList.checks, chk)
		}
	}
	if len(s.checkList.checks) == 0 {
		return errors.New("no matched candidate found")
	}
	//priority from high to low. not stable
	sort.Stable(s.checkList)
	s.pruneCheckList()
	return nil
}

/* Since an agent cannot sendData requests directly from a reflexive
 * candidate, but only from its base, the agent next goes through the
 * sorted list of candidate pairs.  For each pair where the local
 * candidate is server reflexive, the server reflexive candidate MUST be
 * replaced by its base.  Once this has been done, the agent MUST prune
 * the list.  This is done by removing a pair if its local and remote
 * candidates are identical to the local and remote candidates of a pair
 * higher up on the priority list.  The result is a sequence of ordered
 * candidate pairs, called the check list for that media stream.
 */
func (s *IceSession) pruneCheckList() {
	m := make(map[string]bool)
	var checks []*SessionCheck
	for _, c := range s.checkList.checks {
		key := fmt.Sprintf("%d%d", c.localCandidate.Foundation, c.remoteCandidate.addr)
		if m[key] {
			continue
		}
		m[key] = true
		checks = append(checks, c)
	}
	s.checkList.checks = checks
}

/*
如何处理新的数据到来的通知.
来自其他人的 binding request,是必须包含短期认证的,否则可能出现错误
B.4. Importance of the STUN Username

ICE requires the usage of message integrity with STUN using its short-term credential functionality.
The actual short-term credential is formed by exchanging username fragments in the SDP offer/answer exchange.
The need for this mechanism goes beyond just security; it is actually required for correct operation
of ICE in the first place.
*/
func (s *IceSession) StartServer() (err error) {
	defer func() {
		if err != nil {
			for _, srv := range s.serverSocks {
				srv.Close()
			}
		}
	}()
	s.transporter.Close() //首先要关闭这个连接,否则没法再次 Listen, 会提示被占用
	turnsock, hasRelay := s.transporter.(*TurnSock)
	start := 0
	candidates := s.transporter.GetListenCandidiates()
	if hasRelay {
		start = 1
		cfg := &TurnServerSockConfig{
			user:         turnsock.user,
			password:     turnsock.password,
			nonce:        turnsock.nonce,
			realm:        turnsock.realm,
			credentials:  turnsock.credentials,
			relayAddress: turnsock.relayAddress,
			serverAddr:   turnsock.serverAddr,
			lifetime:     turnsock.lifetime,
		}
		s.turnServerSock, err = NewTurnServerSockWrapper(candidates[0], s.Name, s, cfg)
		if err != nil {
			return err
		}
		s.serverSocks[candidates[0]] = s.turnServerSock
	}
	for ; start < len(candidates); start++ {
		var srv *StunServerSock
		srv, err = NewStunServerSock(candidates[start], s, s.Name)
		if err != nil {
			return err
		}
		s.serverSocks[candidates[start]] = srv
	}
	go s.loop()
	return
}

/*
如果需要,发送 create permission 到 turn server, 保证后续经过 turn server 发送的消息发送的出去.
*/
func (s *IceSession) createTurnPermissionIfNeeded() (err error) {
	var res *stun.Message
	if s.turnServerSock != nil {
		res, err = s.turnServerSock.createPermission(s.remoteCandidates)
		if err != nil {
			return
		}
		if res.Type != turn.CreatePermissionResponse {
			return errors.New("Create permission error")
		}
	}
	/*
			1. to keep alive .
			2. refresh permission... turn.lifetime...
		todo
	*/
	return nil
}

/*
check stage:
one check received a valid response .and response.
*/
func (s *IceSession) handleCheckResponse(check *SessionCheck, from string, res *stun.Message) {
	var err error
	log.Trace("%s handle check response check=%s\nfrom=%s\n res=%s", s.Name, check.String(), from, res.String())
	if from != check.remoteCandidate.addr {
		log.Info("%s check received stun message not from expected address,got:%s,check is %s", s.Name, from, check)
	}
	key := fmt.Sprintf("%s-%s", check.localCandidate.addr, check.remoteCandidate.addr)
	if s.iceStreamTransport.State > TransportStateNegotiation {
		log.Info("%s %s received checkresponse ,but check is already finished.", s.Name, key)
		return
	}
	if s.getMsgCheck(res.TransactionID) == nil {
		/*
			因为某些原因,认为这个 check 已经失败了,
			比如超时,直接丢弃即可.
		*/
		return
	}
	if res.Type.Class == stun.ClassErrorResponse {
		log.Info("%s %s received error response %s", s.Name, key, res.Type)
		var code stun.ErrorCodeAttribute
		err := code.GetFrom(res)
		if err != nil || code.Code != stun.CodeRoleConflict {
			s.changeCheckState(check, CheckStateFailed, fmt.Errorf("unkown error code %s", code))
			s.tryCompleteCheck(check)
			return
		}
		/* Role conclict response.
		 *
		 * 7.1.2.1.  Failure Cases:
		 *
		 * If the request had contained the ICE-CONTROLLED attribute,
		 * the agent MUST switch to the controlling role if it has not
		 * already done so.  If the request had contained the
		 * ICE-CONTROLLING attribute, the agent MUST switch to the
		 * controlled role if it has not already done so.  Once it has
		 * switched, the agent MUST immediately retry the request with
		 * the ICE-CONTROLLING or ICE-CONTROLLED attribute reflecting
		 * its new role.
		 */
		var newrole SessionRole = SessionRoleUnkown
		_, err = res.Get(stun.AttrICEControlled)
		if err == nil {
			newrole = SessionRoleControlling
		} else if _, err = res.Get(stun.AttrICEControlling); err == nil {
			newrole = SessionRoleControlled
		}
		if newrole != s.role {
			log.Trace("%s change role from %s to %s", s.Name, s.role, newrole)
			s.role = newrole
		}
		s.retryOneCheck(check)
		return
	}
	/* 7.1.2.1.  Failure Cases
	 *
	 * The agent MUST check that the source IP address and port of the
	 * response equals the destination IP address and port that the Binding
	 * Request was sent to, and that the destination IP address and port of
	 * the response match the source IP address and port that the Binding
	 * Request was sent from.  如何发现peer reflex address? bai
	 */
	if check.remoteCandidate.addr != from {
		err = fmt.Errorf("check %s got message from unkown address %s", check.key, from)
		log.Error("%s connectivity check failed,  check:%s remote address mismatch,err:%s", s.Name, check, err)
		s.changeCheckState(check, CheckStateFailed, err)
		s.tryCompleteCheck(check) //is this the last check?
		return
	}
	/* 7.1.2.2.  Success Cases
	 *
	 * A check is considered to be a success if all of the following are
	 * true:
	 *
	 * o  the STUN transaction generated a success response
	 *
	 * o  the source IP address and port of the response equals the
	 *    destination IP address and port that the Binding Request was sent
	 *    to
	 *
	 * o  the destination IP address and port of the response match the
	 *    source IP address and port that the Binding Request was sent from
	 */
	var xaddr stun.XORMappedAddress
	err = xaddr.GetFrom(res)
	if err != nil {
		s.changeCheckState(check, CheckStateFailed, err)
		s.tryCompleteCheck(check)
		return
	}
	log.Trace("%s get xaddr =%s ", s.Name, xaddr.String())

	var lcand *Candidate
	for _, c := range s.localCandidates {
		if xaddr.String() == c.addr && c.baseAddr == check.localCandidate.baseAddr {
			lcand = c
			break
		}
	}
	if lcand == nil {
		/* 7.1.2.2.1.  Discovering Peer Reflexive Candidates
		 * If the transport address returned in XOR-MAPPED-ADDRESS does not match
		 * any of the local candidates that the agent knows about, the mapped
		 * address represents a new candidate - a peer reflexive candidate.
		 */
		foundation := calcFoundation(check.localCandidate.baseAddr)
		lcand = new(Candidate)
		lcand.Foundation = foundation
		lcand.baseAddr = check.localCandidate.baseAddr
		lcand.Type = CandidatePeerReflexive
		lcand.ComponentID = check.localCandidate.ComponentID
		lcand.addr = xaddr.String()
		lcand.transport = check.localCandidate.transport
		lcand.Priority = calcCandidatePriority(lcand.Type, DefaultPreference, lcand.ComponentID)
		log.Trace("%s candidate add peer reflexive :%s", s.Name, lcand)
		s.localCandidates = append(s.localCandidates, lcand)
	}
	/* 7.1.2.2.3.  Constructing a Valid Pair
	 * Next, the agent constructs a candidate pair whose local candidate
	 * equals the mapped address of the response, and whose remote candidate
	 * equals the destination address to which the request was sent.
	 */

	/* Add pair to valid list, if it's not there, otherwise just update
	 * nominated flag
	 */
	found := false
	var newcheck *SessionCheck
	for _, check2 := range s.validCheckList.checks {
		if check2.localCandidate == lcand && check2.remoteCandidate == check.remoteCandidate {
			found = true
			check2.nominated = check.nominated
			newcheck = check2
			break
		}
	}
	if !found {
		newcheck = &SessionCheck{
			localCandidate:  lcand,
			remoteCandidate: check.remoteCandidate,
			priority:        calcPairPriority(s.role, lcand, check.remoteCandidate),
			state:           CheckStateSucced,
			nominated:       check.nominated,
			key:             fmt.Sprintf("%s-%s", lcand.addr, check.remoteCandidate.addr),
		}
		s.validCheckList.checks = append(s.validCheckList.checks, newcheck)
		sort.Sort(s.validCheckList)
	}
	//find valid check and nominated check
	s.markValidAndNonimated(newcheck)
	/* 7.1.2.2.2.  Updating Pair States
	 *
	 * The agent sets the state of the pair that generated the check to
	 * Succeeded.  The success of this check might also cause the state of
	 * other checks to change as well.
	 */
	s.changeCheckState(check, CheckStateSucced, nil)
	/* Perform 7.1.2.2.2.  Updating Pair States.
	 * This may terminate ICE processing.
	 */
	s.tryCompleteCheck(check)
}
func (s *IceSession) markValidAndNonimated(check *SessionCheck) {
	if s.sessionComponent.validCheck == nil || s.sessionComponent.validCheck.priority < check.priority {
		s.sessionComponent.validCheck = check
	}
	if check.nominated {
		if s.sessionComponent.nominatedCheck == nil || s.sessionComponent.nominatedCheck.priority < check.priority {
			s.sessionComponent.nominatedCheck = check
		}
	}
}

/*
if all check is failed or success, notify upper layer. return true, when this ice negotiation finished.
*/
func (s *IceSession) tryCompleteCheck(check *SessionCheck) bool {
	/* 7.1.2.2.2.  Updating Pair States
	 *
	 * The agent sets the state of the pair that generated the check to
	 * Succeeded.  The success of this check might also cause the state of
	 * other checks to change as well.  The agent MUST perform the following
	 * two steps:
	 *
	 * 1.  The agent changes the states for all other Frozen pairs for the
	 *     same media stream and same foundation to Waiting.  Typically
	 *     these other pairs will have different component IDs but not
	 *     always.
	 */
	if check.err == nil {
		for _, c := range s.checkList.checks {
			if c.localCandidate.Foundation == check.localCandidate.Foundation && c.state == CheckStateFrozen {
				s.changeCheckState(c, CheckStateWaiting, nil)
			}
		}
		log.Trace("%s check  finished:%s", s.Name, check.String())
	}

	/* 8.2.  Updating States
	 *
	 * For both controlling and controlled agents, the state of ICE
	 * processing depends on the presence of nominated candidate pairs in
	 * the valid list and on the state of the check list:
	 *
	 * o  If there are no nominated pairs in the valid list for a media
	 *    stream and the state of the check list is Running, ICE processing
	 *    continues.
	 *
	 * o  If there is at least one nominated pair in the valid list:
	 *
	 *    - The agent MUST remove all Waiting and Frozen pairs in the check
	 *      list for the same component as the nominated pairs for that
	 *      media stream
	 *
	 *    - If an In-Progress pair in the check list is for the same
	 *      component as a nominated pair, the agent SHOULD cease
	 *      retransmissions for its check if its pair priority is lower
	 *      than the lowest priority nominated pair for that component
	 */
	if check.err == nil && check.nominated {
		for _, c := range s.checkList.checks {
			if c.state < CheckStateInProgress {
				//just fail frozen/waiting check
				log.Trace("%s check %s to be failed because higher priority check finished.", s.Name, c.key)
				s.cancelOneCheck(c)
				//s.changeCheckState(c, CheckStateFailed, errors.New("canceled"))
			} else if c.state == CheckStateInProgress /* && c.priority < check.priority*/ {
				/*
					这种策略会尽快结束,但是存在问题,如果低优先级的先完成
					1. 对方可能会收到高优先级的 request, 进而以高优先级为准,如果只有一个 ip 地址,那没什么问题
					2. 应该被采用的高优先级被放弃.
				*/
				/* State is IN_PROGRESS, cancel transaction */
				s.cancelOneCheck(c)
			}
		}
	}
	/* Still in 8.2.  Updating States
	 *
	 * o  Once there is at least one nominated pair in the valid list for
	 *    every component of at least one media stream and the state of the
	 *    check list is Running:
	 *
	 *    *  The agent MUST change the state of processing for its check
	 *       list for that media stream to Completed.
	 *
	 *    *  The agent MUST continue to respond to any checks it may still
	 *       receive for that media stream, and MUST perform triggered
	 *       checks if required by the processing of Section 7.2.
	 *
	 *    *  The agent MAY begin transmitting media for this media stream as
	 *       described in Section 11.1
	 */
	/*
		only one component,so finish
	*/
	//todo notify ice success..
	if s.sessionComponent.nominatedCheck != nil {
		s.iceComplete(nil)
		return true
	}
	/* Note: this is the stuffs that we don't do in 7.1.2.2.2, since our
	 *       ICE session only supports one media stream for now:
	 *
	 * 7.1.2.2.2.  Updating Pair States
	 *
	 * 2.  If there is a pair in the valid list for every component of this
	 *     media stream (where this is the actual number of components being
	 *     used, in cases where the number of components signaled in the SDP
	 *     differs from offerer to answerer), the success of this check may
	 *     unfreeze checks for other media streams.
	 */

	/* 7.1.2.3.  Check List and Timer State Updates
	 * Regardless of whether the check was successful or failed, the
	 * completion of the transaction may require updating of check list and
	 * timer states.
	 *
	 * If all of the pairs in the check list are now either in the Failed or
	 * Succeeded state, and there is not a pair in the valid list for each
	 * component of the media stream, the state of the check list is set to
	 * Failed.
	 */

	/*
	 * See if all checks in the checklist have completed. If we do,
	 * then mark ICE processing as failed.
	 */
	hasNotFinished := false
	for _, c := range s.checkList.checks {
		if c.state < CheckStateSucced {
			hasNotFinished = true
			break
		}
	}
	if !hasNotFinished {
		/* All checks have completed, but we don't have nominated pair.
		 * If agent's role is controlled, check if all components have
		 * valid pair. If it does, this means the controlled agent has
		 * finished the check list and it's waiting for controlling
		 * agent to sendData checks with USE-CANDIDATE flag set.
		 */
		if s.role == SessionRoleControlled {
			if s.sessionComponent.validCheck == nil {
				//todo notify ice failed.
				s.iceComplete(errors.New("no valid check"))
				return true
			} else {
				log.Trace("%s all checks completed. controlled agent now waits for nomination..", s.Name)
				go func() {
					//start a timer,failed if there is no nomiated
					time.Sleep(time.Second * 10) // time from pjnath
					if s.sessionComponent.nominatedCheck == nil {
						s.iceComplete(errors.New("no nonimated"))
					}
				}()
				return false
			}
		} else if s.isNominating { //如果我是 controlling, 那么总是采用 aggressive策略.
			s.iceComplete(fmt.Errorf("%s controlling no nominated ", s.Name))
			return true
		} else {
			/*
				如果我是 regular 模式,那么此时应该是再次发送 bingdingrequest, 并带上 usecandidate, 目前没必要.
			*/
			panic("not implemented")
			return false
		}

	}
	/* If this connectivity check has been successful, scan all components
	 * and see if they have a valid pair, if we are controlling and we haven't
	 * started our nominated check yet.
	 */
	//目前只有一个 component, 另外只支持 aggressive 模式.
	return false
}
func (s *IceSession) iceComplete(result error) {
	//通知调用者,完毕, 但是这个可能不是最终的选项?
	//if len(s.checkMap) != 0 {
	//	panic("all check should finished")
	//}
	//应该继续允许处理 BindingRequest, 因为对方可能还没有结束.
	log.Debug("icesseion %s complete ,err:%s", s.Name, result)
	if result != nil {

	} else {
		log.Trace("valid check=%s\n nominated=%s\n", s.sessionComponent.validCheck, s.sessionComponent.nominatedCheck)

	}
	s.iceStreamTransport.onIceComplete(result)
}

/*
cancel one started check
*/
func (s *IceSession) cancelOneCheck(check *SessionCheck) {
	//if check.state != CheckStateInProgress {
	//	log.Info("can only cancel a in progress check, state=%s", check.state)
	//	return
	//}
	chr := s.checkMap[check.key]
	chr <- errors.New("canceled")
	s.changeCheckState(check, CheckStateFailed, errors.New("canceled"))
}
func (s *IceSession) finishOneCheck(check *SessionCheck) {
	chr := s.checkMap[check.key]
	delete(s.checkMap, check.key)
	close(chr)
}
func (s *IceSession) retryOneCheck(check *SessionCheck) {
	if check.state != CheckStateInProgress {
		log.Info("only can retry a check in progress")
		return
	}
	chr := s.checkMap[check.key]
	chr <- errCheckRetry
}
func (s *IceSession) startCheck() error {
	log.Trace("start ice check...")
	if s.aggresive && s.role == SessionRoleControlling {
		s.isNominating = true
	}
	if s.checkList.checks[0].state != CheckStateFrozen {
		return errors.New("already start another check...")
	}
	s.allcheck(s.checkList.checks)
	return nil
}
func (s *IceSession) changeCheckState(check *SessionCheck, newState SessionCheckState, err error) {
	log.Trace("%s check %s: state changed from %s to %s err:%s", s.Name, check.key, check.state, newState, err)
	if check.state >= newState {
		log.Error("%s check state only can increase. newstate=%s check=%s", s.Name, newState, check)
		return
	}
	check.state = newState
	check.err = err
	//停止探测
	if check.state >= CheckStateSucced {
		s.finishOneCheck(check)
	}
}

//启动完毕以后立即返回,结果要从 ice complete中获取.
func (s *IceSession) allcheck(checks []*SessionCheck) {
	const checkInterval = time.Millisecond * 20
	for _, c := range checks {
		key := fmt.Sprintf("%s-%s", c.localCandidate.addr, c.remoteCandidate.addr)
		ch := make(chan error, 1)
		s.checkMap[key] = ch
	}
	/*
		only one compondent, all waiting...
	*/
	for _, c := range checks {
		s.changeCheckState(c, CheckStateWaiting, nil)
	}
	for _, rc := range s.earlyCheckList {
		/*
			优先处理收到的请求,可能已经可以成功了.
		*/
		log.Trace("%s process early check list %s", s.Name, log.StringInterface(rc, 3))
		s.handleIncomingCheck(rc)
	}
	for _, c := range checks {
		ch := s.checkMap[c.key]
		//有可能还没有启动,其他 check 已经完毕,这个就没有必要了.
		if c.state == CheckStateWaiting {
			s.changeCheckState(c, CheckStateInProgress, nil)
			go s.onecheck(c, ch, s.isNominating)
			time.Sleep(checkInterval)
		}
	}
}
func (s *IceSession) buildBindingRequest(c *SessionCheck) (req *stun.Message) {
	var (
		err      error
		priority attr.Priority
		control  stun.Setter
		prio     int
		setters  []stun.Setter
	)
	req = new(stun.Message)
	prio = calcCandidatePriority(CandidatePeerReflexive, DefaultPreference, 1)
	priority = attr.Priority(prio)
	if s.role == SessionRoleControlling {
		control = attr.IceControlling(s.tieBreaker)
	} else {
		control = attr.IceControlled(s.tieBreaker)
	}
	setters = []stun.Setter{stun.BindingRequest,
		stun.TransactionIDSetter,
		priority, control,
		software,
		stun.Username(s.txUserName),
		s.txCrendientials,
		stun.Fingerprint}
	if c.nominated {
		//useCandidate 不能放在最后,
		setters = append([]stun.Setter{attr.UseCandidate}, setters...)
	}
	err = req.Build(setters...)
	if err != nil {
		panic("build error...")
	}
	return
}
func (s *IceSession) getSenderServerSock(localAddr string) ServerSocker {
	srv, ok := s.serverSocks[localAddr]
	if ok {
		return srv
	}
	return s.turnServerSock
}
func (s *IceSession) onecheck(c *SessionCheck, chCheckResult chan error, nominate bool) {
	var (
		err        error
		req        *stun.Message
		sleep      time.Duration = time.Millisecond * 100
		serversock ServerSocker
	)
	log.Trace("%s start check %s", s.Name, c.key)
	serversock = s.getSenderServerSock(c.localCandidate.addr)
	if nominate {
		c.nominated = true
	}
	//build req message
lblRestart:
	req = s.buildBindingRequest(c)
	for i := 0; i < 7; i++ {
		log.Trace("%s %s sendData %d times", s.Name, c.key, i+1)
		req.NewTransactionID()
		s.addMsgCheck(req.TransactionID, c)
		err = serversock.sendStunMessageAsync(req, c.localCandidate.addr, c.remoteCandidate.addr)
		if err != nil {
			//goto lblErr
			time.Sleep(sleep)
			continue
		}
		select {
		case <-time.After(sleep):
			sleep += time.Millisecond * 100
			continue
		case err = <-chCheckResult:
			if err == errCheckRetry {
				goto lblRestart // 立即进行下一次探测.
			}
			return
		}
	}
	//探测了七次,没有任何结果,失败.
	err = errTriedTooManyTimes
	s.changeCheckState(c, CheckStateFailed, err)
	s.tryCompleteCheck(c)
	return
}

func (s *IceSession) changeRole(newrole SessionRole) {
	log.Trace("%s role changed from % to %s", s.Name, s.role, newrole)
	s.role = newrole
}
func (s *IceSession) sendResponse(localAddr, fromAddr string, req *stun.Message, code stun.ErrorCode) {
	var (
		err         error
		res         *stun.Message = new(stun.Message)
		fromUdpAddr *net.UDPAddr
	)
	fromUdpAddr = addrToUdpAddr(fromAddr)
	sc := s.getSenderServerSock(localAddr)
	if code == 0 {
		err = res.Build(
			stun.NewTransactionIDSetter(req.TransactionID),
			stun.NewType(stun.MethodBinding, stun.ClassSuccessResponse),
			software,
			&stun.XORMappedAddress{
				IP:   fromUdpAddr.IP,
				Port: fromUdpAddr.Port,
			},
			s.txCrendientials,
			stun.Fingerprint,
		)
		if err != nil {
			panic(fmt.Sprintf("build res message error %s", err))
		}
		sc.sendStunMessageAsync(res, localAddr, fromAddr)
		return
	} else if code == stun.CodeRoleConflict {
		err = res.Build(
			stun.NewTransactionIDSetter(req.TransactionID),
			stun.NewType(stun.MethodBinding, stun.ClassErrorResponse),
			software,
			stun.CodeRoleConflict,
			&stun.XORMappedAddress{
				IP:   fromUdpAddr.IP,
				Port: fromUdpAddr.Port,
			},
			s.txCrendientials,
			stun.Fingerprint,
		)
		if err != nil {
			panic(fmt.Sprintf("build res message error %s", err))
		}
		sc.sendStunMessageAsync(res, localAddr, fromAddr)
		return
	} else if code == stun.CodeUnauthorised {
		res.Build(stun.NewTransactionIDSetter(req.TransactionID), stun.BindingError,
			stun.CodeUnauthorised, software, s.txCrendientials, stun.Fingerprint)
		sc.sendStunMessageAsync(res, localAddr, fromAddr)
	}
}

//binding request 和普通的 stun message 一样处理.
func (s *IceSession) processBindingRequest(localAddr, fromAddr string, req *stun.Message) {
	var (
		err         error
		hasControll          = false
		rcheck      *RxCheck = new(RxCheck)
		priority    attr.Priority
	)
	var userName stun.Username
	log.Trace("%s received binding request  %s<----------%s", s.Name, localAddr, fromAddr)
	err = priority.GetFrom(req)
	if err != nil {
		log.Info("stun bind request has no priority,ingored.")
		return
	}
	rcheck.priority = int(priority)
	err = userName.GetFrom(req)
	if err != nil {
		log.Info("%s received bind request  with no username %s", localAddr, err)
		s.sendResponse(localAddr, fromAddr, req, stun.CodeUnauthorised)
		return
	}
	_, err = req.Get(stun.AttrICEControlling)
	if err == nil {
		hasControll = true
		rcheck.role = SessionRoleControlling
		if s.role != SessionRoleControlled {
			var peerTieBreaker attr.IceControlling
			peerTieBreaker.GetFrom(req)
			/*
				tiebreaker, 谁的大以谁的为准.
			*/
			if s.tieBreaker < uint64(peerTieBreaker) {
				s.changeRole(SessionRoleControlled)
			} else {
				s.sendResponse(localAddr, fromAddr, req, stun.CodeRoleConflict)
				return
			}

		}
	}
	_, err = req.Get(stun.AttrICEControlled)
	if err == nil {
		hasControll = true
		rcheck.role = SessionRoleControlled
		if s.role != SessionRoleControlling {
			var peerTieBreaker attr.IceControlled
			peerTieBreaker.GetFrom(req)
			if s.tieBreaker < uint64(peerTieBreaker) {
				s.changeRole(SessionRoleControlling)
			} else {
				s.sendResponse(localAddr, fromAddr, req, stun.CodeRoleConflict)
				return
			}

		}
	}
	if !hasControll {
		log.Info("%s received stun binding request,but no icecontrolling and icecontrolled", s.Name)
		s.sendResponse(localAddr, fromAddr, req, stun.CodeUnauthorised)
		return
	}
	/*
		如果是 earlycheck, 那么发送过去的 response 中 username 应该是错的,所以我们不能认为 username 不对就是错的.
	*/
	s.sendResponse(localAddr, fromAddr, req, 0)
	if s.iceStreamTransport.State >= TransportStateRunning {
		return // 不应该继续处理了,因为negotiation 已经完成了.
	}
	//early check received.
	if s.txCrendientials == nil {
		s.rxUserName = string(userName)
		log.Info("%s received early check from %s, username=%s", s.Name, fromAddr, s.rxUserName)
	}
	/*
	 * Handling early check.
	 *
	 * It's possible that we receive this request before we receive SDP
	 * answer. In this case, we can't perform trigger check since we
	 * don't have checklist yet, so just save this check in a pending
	 * triggered check array to be acted upon later.
	 */
	//init check
	_, err = req.Get(stun.AttrUseCandidate)
	if err == nil {
		rcheck.userCandidate = true
	}
	rcheck.componentId = 1
	rcheck.remoteAddress = fromAddr
	rcheck.localAddress = localAddr
	if s.txCrendientials == nil {
		/*
			We don't have answer yet, so keep this request for later
		*/
		s.earlyCheckList = append(s.earlyCheckList, rcheck)
	} else {
		//其他阶段忽略,我已经选定了用于通信的 check
		s.handleIncomingCheck(rcheck)

	}
}

/* Handle incoming Binding request and perform triggered check.
 * This function may be called by processBindingRequest, or when
 * SDP answer is received and we have received early checks.
 */

func (s *IceSession) handleIncomingCheck(rcheck *RxCheck) {
	var (
		lcand *Candidate
		rcand *Candidate
	)
	/* 7.2.1.3.  Learning Peer Reflexive Candidates
	 * If the source transport address of the request does not match any
	 * existing remote candidates, it represents a new peer reflexive remote
	 * candidate.
	 */
	for _, c := range s.remoteCandidates {
		if c.addr == rcheck.remoteAddress {
			rcand = c
			break
		}
	}
	if rcand == nil {
		if len(s.remoteCandidates) > MaxCandidates {
			log.Warn("%s unable to add new peer reflexive candidate: too many candidates .", s.Name)
			return
		}
		rcand = new(Candidate)
		rcand.ComponentID = 1
		rcand.Type = CandidatePeerReflexive
		rcand.Priority = rcheck.priority
		rcand.addr = rcheck.remoteAddress
		rcand.Foundation = calcFoundation(rcheck.remoteAddress)
		s.remoteCandidates = append(s.remoteCandidates, rcand)
		log.Info("%s add new remote candidate from the request %s", s.Name, rcand.addr)
	}
	/*
		寻找匹配这个 rcheck 的 localCandidates, 就找优先级最高的那个就可以了.
	*/
	for _, cand := range s.localCandidates {
		if cand.addr == rcheck.localAddress {
			lcand = cand
			break
		}
	}
	/*
	 * Create candidate pair for this request.
	 */

	/*
	 * 7.2.1.4.  Triggered Checks
	 *
	 * Now that we have local and remote candidate, check if we already
	 * have this pair in our checklist.
	 */
	var c *SessionCheck
	for _, chk := range s.checkList.checks {
		if chk.localCandidate == lcand && chk.remoteCandidate == rcand {
			c = chk
			break
		}
	}
	/* If the pair is already on the check list:
	 * - If the state of that pair is Waiting or Frozen, its state is
	 *   changed to In-Progress and a check for that pair is performed
	 *   immediately.  This is called a triggered check.
	 *
	 * - If the state of that pair is In-Progress, the agent SHOULD
	 *   generate an immediate retransmit of the Binding Request for the
	 *   check in progress.  This is to facilitate rapid completion of
	 *   ICE when both agents are behind NAT.
	 *
	 * - If the state of that pair is Failed or Succeeded, no triggered
	 *   check is sent.
	 */
	if c != nil {
		oldnominated := c.nominated
		c.nominated = rcheck.userCandidate || c.nominated
		log.Trace("%s change check %s nominated from %v to %v", s.Name, c.key, oldnominated, c.nominated)
		if c.state == CheckStateFrozen || c.state == CheckStateWaiting {
			log.Trace("performing triggered check for %s", c.key)
			chResult, ok := s.checkMap[c.key]
			if !ok {
				panic("must ...")
			}
			s.changeCheckState(c, CheckStateInProgress, nil)
			go s.onecheck(c, chResult, c.nominated || s.isNominating)
		} else if c.state == CheckStateInProgress {
			//Should retransmit immediately
			log.Trace("%s triggered check for check %s not performed, because its in progress. Retransmitting", s.Name, c.key)
			s.retryOneCheck(c)
		} else if c.state == CheckStateSucced {
			if rcheck.userCandidate {
				for _, vc := range s.validCheckList.checks {
					if vc.remoteCandidate == c.remoteCandidate {
						vc.nominated = true
						s.markValidAndNonimated(vc)
						log.Trace("%s valid check %s is nominated", s.Name, vc.key)
					}
				}
			}
			log.Trace("%s triggered check for check %s not performed because it's completed", s.Name, c.key)
			complete := s.tryCompleteCheck(c)
			if complete {
				return
			}
		}
	} else {
		/* If the pair is not already on the check list:
		 * - The pair is inserted into the check list based on its priority.
		 * - Its state is set to In-Progress
		 * - A triggered check for that pair is performed immediately.
		 */
		/* Note: only do this if we don't have too many checks in checklist */
		c := &SessionCheck{
			localCandidate:  lcand,
			remoteCandidate: rcand,
			priority:        calcPairPriority(s.role, lcand, rcand),
			state:           CheckStateWaiting,
			nominated:       rcheck.userCandidate,
			key:             fmt.Sprintf("%s-%s", lcand.addr, rcand.addr),
		}
		s.checkList.checks = append(s.checkList.checks, c)
		log.Trace("%s New triggered check added:%s", s.Name, c.key)
	}
}

/**
 * This structure represents an incoming check (an incoming Binding
 * request message), and is mainly used to keep early checks in the
 * list in the ICE session. An early check is a request received
 * from remote when we haven't received SDP answer yet, therefore we
 * can't perform triggered check. For such cases, keep the incoming
 * request in a list, and we'll do triggered checks (simultaneously)
 * as soon as we receive answer.
 */
type RxCheck struct {
	componentId   int
	remoteAddress string
	localAddress  string
	userCandidate bool
	priority      int
	role          SessionRole
}

func (s *IceSession) processBindingResponse(localAddr, remoteAddr string, msg *stun.Message) {
	id := msg.TransactionID
	check := s.getMsgCheck(id)
	if check == nil {
		log.Info("%s receive bind response ,but has no related check %s", s.Name, msg)
		return
	}
	if check.localCandidate.addr != localAddr {
		log.Warn("%s received bind response ,but local addr err ,expect %s,got %s", s.Name, check.localCandidate.addr, localAddr)
		return
	}
	s.handleCheckResponse(check, remoteAddr, msg)
}

type stunMessageWrapper struct {
	localAddr  string
	remoteAddr string
	msg        *stun.Message
}
type stunDataWrapper struct {
	localAddr  string
	remoteAddr string
	data       []byte
}

func (s *IceSession) loop() {
	for {
		select {
		case msg, ok := <-s.msgChan:
			if ok {
				s.processStunMessage(msg.localAddr, msg.remoteAddr, msg.msg)
			} else {
				return
			}
		case data, ok := <-s.dataChan:
			if ok {
				s.iceStreamTransport.onRxData(data.data, data.remoteAddr)
			} else {
				return
			}
		}
	}
}
func (s *IceSession) processStunMessage(localAddr, remoteAddr string, msg *stun.Message) {
	if msg.Type == stun.BindingRequest {
		s.processBindingRequest(localAddr, remoteAddr, msg)
		return
	}
	//binding response?
	if msg.Type == stun.BindingError || msg.Type == stun.BindingSuccess {
		s.processBindingResponse(localAddr, remoteAddr, msg)
		return
	}
	log.Warn("%s %s receive unexpected stun message from  %s, msg:%s", s.Name, localAddr, remoteAddr, msg.Type)
	////if s.iceStreamTransport.State == TransportStateNegotiation {
	//res := new(stun.Message)
	//if msg.Type != stun.BindingSuccess && msg.Type != stun.BindingError {
	//	err := res.Build(stun.NewTransactionIDSetter(msg.TransactionID),
	//		stun.BindingError,
	//		stun.CodeBadRequest,
	//		stun.Fingerprint,
	//	)
	//	if err != nil {
	//		panic("error")
	//	}
	//	s.getSenderServerSock(localAddr).sendStunMessageAsync(res, localAddr, remoteAddr)
	//	return
	//}
}

/*
message received from peer or stun server after negiotiation complete.
*/
func (s *IceSession) RecieveStunMessage(localAddr, remoteAddr string, msg *stun.Message) {
	log.Trace("%s %s receive stun message from  %s, msg:%s", s.Name, localAddr, remoteAddr, msg.Type)
	if s.hasStopped {
		return
	}
	//不要阻塞发送接收消息线程.
	s.msgChan <- &stunMessageWrapper{localAddr, remoteAddr, msg}
	return

}

/*
	ICE 协商建立连接以后,收到了对方发过来的数据,可能是经过 turn server 中转的 channel data( 不接受 sendData data request),也可能直接是数据.
	如果是经过 turn server 中转的, channelNumber 一定介于0x4000-0x7fff 之间.否则一定为0
*/
func (s *IceSession) ReceiveData(localAddr, peerAddr string, data []byte) {
	if s.hasStopped {
		return
	}
	s.dataChan <- &stunDataWrapper{localAddr, peerAddr, data}
	return

}

/*
pair priority = 2^32*MIN(G,D) + 2*MAX(G,D) + (G>D?1:0)
*/
func calcPairPriority(role SessionRole, l, r *Candidate) int64 {
	var o, a int32
	var min, max int32
	if role == SessionRoleControlling {
		o = int32(l.Priority)
		a = int32(r.Priority)
	} else {
		o = int32(r.Priority)
		a = int32(l.Priority)
	}
	if o > a {
		min = a
		max = o
	} else {
		min = o
		max = a
	}
	var p int64
	p = int64(min) << 32
	max = max << 1
	p += int64(32)
	if o > a {
		p += 1
	}
	return p
}
