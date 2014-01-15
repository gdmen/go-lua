package lua

import "math"

const (
	oprMinus = iota
	oprNot
	oprLength
	oprNoUnary
)

const (
	noJump     = -1
	noRegister = maxArgA
)

const (
	oprAdd = iota
	oprSub
	oprMul
	oprDiv
	oprMod
	oprPow
	oprConcat
	oprEq
	oprLT
	oprLE
	oprNE
	oprGT
	oprGE
	oprAnd
	oprOr
	oprNoBinary
)

const (
	kindVoid = iota // no value
	kindNil
	kindTrue
	kindFalse
	kindConstant       // info = index of constant
	kindNumber         // value = numerical value
	kindNonRelocatable // info = result register
	kindLocal          // info = local register
	kindUpValue        // info = index of upvalue
	kindIndexed        // table = table register/upvalue, index = register/constant index
	kindJump           // info = instruction pc
	kindRelocatable    // info = instruction pc
	kindCall           // info = instruction pc
	kindVarArg         // info = instruction pc
)

type exprDesc struct {
	kind      int
	index     int // register/constant index
	table     int // register or upvalue
	tableType int // whether 'table' is register (kindLocal) or upvalue (kindUpValue)
	info      int
	t, f      int // patch lists for 'exit when true/false'
	value     float64
}

type function struct {
	f                      *prototype
	h                      *table
	previous               *function
	p                      *parser
	pc, jumpPC, lastTarget int
	isVarArg               bool
	code                   []instruction
	constants              []value
	lines                  []int
	freeRegisterCount      int
	activeVariableCount    int
	maxStackSize           int
	// TODO ...
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func not(b int) int {
	if b == 0 {
		return 1
	}
	return 0
}

// func (f *function) enterBlock(isLoop bool) *block {
//   f.block = &block{isLoop: isLoop, activeVariableCount: f.activeVariableCount, firstLabel: f.p.dynamicData.label, firstGoto: f.p.dynamicData.goto, previous: f.block}
//   f.p.state.assert(f.freeRegisters == f.activeVariableCount)
//   return f.block
// }

func makeExpression(kind, info int) exprDesc {
	return exprDesc{f: noJump, t: noJump, kind: kind, info: info}
}

func (f *function) unreachable()                        { f.p.l.assert(false) }
func (f *function) assert(cond bool)                    { f.p.l.assert(cond) }
func (f *function) instruction(e exprDesc) *instruction { return &f.code[e.info] }
func (e exprDesc) hasJumps() bool                       { return e.t != e.f }
func (e exprDesc) isNumeral() bool                      { return e.kind == kindNumber && e.t == noJump && e.f == noJump }

func (f *function) Encode(i instruction) int {
	f.dischargeJumpPC()
	f.f.code = append(f.f.code, i) // TODO check that we always only append
	f.f.lineInfo = append(f.f.lineInfo, int32(f.p.lastLine))
	f.pc++
	return f.pc
}

func (f *function) EncodeABC(op opCode, a, b, c int) int {
	f.assert(opMode(op) == iABC)
	f.assert(bMode(op) != opArgN || b == 0)
	f.assert(cMode(op) != opArgN || c == 0)
	f.assert(a <= maxArgA && b <= maxArgB && c <= maxArgC)
	return f.Encode(createABC(op, a, b, c))
}

func (f *function) EncodeABx(op opCode, a, bx int) int {
	f.assert(opMode(op) == iABx || opMode(op) == iAsBx)
	f.assert(cMode(op) == opArgN)
	f.assert(a <= maxArgA && bx <= maxArgBx)
	return f.Encode(createABx(op, a, bx))
}

func (f *function) EncodeAsBx(op opCode, a, sbx int) int {
	return f.EncodeABx(op, a, sbx+maxArgSBx)
}

func (f *function) encodeExtraArg(a int) int {
	f.assert(a <= maxArgAx)
	return f.Encode(createAx(opExtraArg, a))
}

func (f *function) EncodeConstant(r, constant int) int {
	if constant <= maxArgBx {
		return f.EncodeABx(opLoadConstant, r, constant)
	}
	pc := f.EncodeABx(opLoadConstant, r, 0)
	f.encodeExtraArg(constant)
	return pc
}

func (f *function) LoadNil(from, n int) {
	if f.pc > f.lastTarget { // no jumps to current position
		if previous := &f.code[f.pc-1]; previous.opCode() == opLoadNil {
			if pf, pl, l := previous.a(), previous.a()+previous.b(), from+n-1; pf <= from && from < pl || from <= pf && pf < l { // can connect both
				from, l = min(from, pf), max(l, pl)
				previous.setA(from)
				previous.setB(l - from)
				return
			}
		}
	}
	f.EncodeABC(opLoadNil, from, n-1, 0)
}

func (f *function) Jump() int {
	jumpPC := f.jumpPC
	f.jumpPC = noJump
	return f.Concatenate(f.EncodeAsBx(opJump, 0, noJump), jumpPC)
}

func (f *function) JumpTo(target int) {
	f.PatchList(f.Jump(), target)
}

func (f *function) Return(first, count int) {
	f.EncodeABC(opReturn, first, count+1, 0)
}

func (f *function) conditionalJump(op opCode, a, b, c int) int {
	f.EncodeABC(op, a, b, c)
	return f.Jump()
}

func (f *function) fixJump(pc, dest int) {
	f.assert(dest != noJump)
	offset := dest - (pc + 1)
	if abs(offset) > maxArgSBx {
		f.p.syntaxError("control structure too long")
	}
	f.code[pc].setSBx(offset)
}

func (f *function) Label() int {
	f.lastTarget = f.pc
	return f.pc
}

func (f *function) jump(pc int) int {
	if offset := f.code[pc].sbx(); offset != noJump {
		return pc + 1 + offset
	}
	return noJump
}

func (f *function) jumpControl(pc int) *instruction {
	if pc >= 1 && testTMode(f.code[pc-1].opCode()) {
		return &f.code[pc-1]
	}
	return &f.code[pc]
}

func (f *function) needValue(list int) bool {
	for ; list != noJump; list = f.jump(list) {
		if f.jumpControl(list).opCode() != opTestSet {
			return true
		}
	}
	return false
}

func (f *function) patchTestRegister(node, register int) bool {
	if i := f.jumpControl(node); i.opCode() != opTestSet {
		return false
	} else if register != noRegister && register != i.b() {
		i.setA(register)
	} else {
		*i = createABC(opTest, i.b(), 0, i.c())
	}
	return true
}

func (f *function) removeValues(list int) {
	for ; list != noJump; list = f.jump(list) {
		_ = f.patchTestRegister(list, noRegister)
	}
}

func (f *function) patchListHelper(list, target, register, defaultTarget int) {
	for list != noJump {
		next := f.jump(list)
		if f.patchTestRegister(list, register) {
			f.fixJump(list, target)
		} else {
			f.fixJump(list, defaultTarget)
		}
		list = next
	}
}

func (f *function) dischargeJumpPC() {
	f.patchListHelper(f.jumpPC, f.pc, noRegister, f.pc)
	f.jumpPC = noJump
}

func (f *function) PatchList(list, target int) {
	if target == f.pc {
		f.PatchToHere(list)
	} else {
		f.assert(target < f.pc)
		f.patchListHelper(list, target, noRegister, target)
	}
}

func (f *function) PatchClose(list, level int) {
	for level, next := level+1, 0; list != noJump; list = next {
		next = f.jump(list)
		f.assert(f.code[list].opCode() == opJump && f.code[list].a() == 0 || f.code[list].a() >= level)
		f.code[list].setA(level)
	}
}

func (f *function) PatchToHere(list int) {
	f.Label()
	f.jumpPC = f.Concatenate(f.jumpPC, list)
}

func (f *function) Concatenate(l1, l2 int) int {
	switch {
	case l2 == noJump:
	case l1 == noJump:
		return l2
	default:
		list := l1
		for next := f.jump(list); next != noJump; list, next = next, f.jump(next) {
		}
		f.fixJump(list, l2)
	}
	return l1
}

func (f *function) addConstant(k, v value) (index int) {
	if old, ok := f.h.at(k).(float64); ok {
		if index = int(old); f.constants[index] == v {
			return
		}
	}
	index = len(f.constants)
	f.h.put(k, float64(index))
	f.constants = append(f.constants, v)
	return
}

func (f *function) NumberConstant(n float64) int {
	if n == 0.0 && math.Signbit(n) {
		return f.addConstant("-0.0", n)
	} else if n == 0.0 {
		return f.addConstant("0.0", n)
	} else if math.IsNaN(n) {
		return f.addConstant("NaN", n)
	}
	return f.addConstant(n, n)
}

func (f *function) CheckStack(n int) {
	if n += f.freeRegisterCount; n >= maxStack {
		f.p.syntaxError("function or expression too complex")
	} else if n > f.maxStackSize {
		f.maxStackSize = n
	}
}

func (f *function) ReserveRegisters(n int) {
	f.CheckStack(n)
	f.freeRegisterCount += n
}

func (f *function) freeRegister(r int) {
	if !isConstant(r) && r >= f.activeVariableCount {
		f.freeRegisterCount--
		f.assert(r == f.freeRegisterCount)
	}
}

func (f *function) freeExpression(e exprDesc) {
	if e.kind == kindNonRelocatable {
		f.freeRegister(e.info)
	}
}

func (f *function) StringConstant(s string) int { return f.addConstant(s, s) }
func (f *function) booleanConstant(b bool) int  { return f.addConstant(b, b) }
func (f *function) nilConstant() int            { return f.addConstant(f.h, nil) }

func (f *function) SetReturns(e exprDesc, resultCount int) {
	if e.kind == kindCall {
		f.instruction(e).setC(resultCount + 1)
	} else if e.kind == kindVarArg {
		f.instruction(e).setB(resultCount + 1)
		f.instruction(e).setA(f.freeRegisterCount)
		f.ReserveRegisters(1)
	}
}

func (f *function) SetReturn(e exprDesc) exprDesc {
	if e.kind == kindCall {
		e.kind, e.info = kindNonRelocatable, f.instruction(e).a()
	} else if e.kind == kindVarArg {
		f.instruction(e).setB(2)
		e.kind = kindRelocatable
	}
	return e
}

func (f *function) DischargeVariables(e exprDesc) exprDesc {
	switch e.kind {
	case kindLocal:
		e.kind = kindNonRelocatable
	case kindUpValue:
		e.kind, e.info = kindRelocatable, f.EncodeABC(opGetUpValue, 0, e.info, 0)
	case kindIndexed:
		if f.freeRegister(e.index); e.tableType == kindLocal {
			f.freeRegister(e.table)
			e.kind, e.info = kindRelocatable, f.EncodeABC(opGetTable, 0, e.table, e.index)
		} else {
			e.kind, e.info = kindRelocatable, f.EncodeABC(opGetTableUp, 0, e.table, e.index)
		}
	case kindVarArg, kindCall:
		e = f.SetReturn(e)
	}
	return e
}

func (f *function) dischargeToRegister(e exprDesc, r int) exprDesc {
	switch e = f.DischargeVariables(e); e.kind {
	case kindNil:
		f.LoadNil(r, 1)
	case kindFalse:
		f.EncodeABC(opLoadBool, r, 0, 0)
	case kindTrue:
		f.EncodeABC(opLoadBool, r, 1, 0)
	case kindConstant:
		f.EncodeConstant(r, e.info)
	case kindNumber:
		f.EncodeConstant(r, f.NumberConstant(e.value))
	case kindRelocatable:
		f.instruction(e).setA(r)
	case kindNonRelocatable:
		if r != e.info {
			f.EncodeABC(opMove, r, e.info, 0)
		}
	default:
		f.assert(e.kind == kindVoid || e.kind == kindJump)
		return e
	}
	e.kind, e.info = kindNonRelocatable, r
	return e
}

func (f *function) dischargeToAnyRegister(e exprDesc) exprDesc {
	if e.kind != kindNonRelocatable {
		f.ReserveRegisters(1)
		e = f.dischargeToRegister(e, f.freeRegisterCount-1)
	}
	return e
}

func (f *function) encodeLabel(a, b, jump int) int {
	f.Label()
	return f.EncodeABC(opLoadBool, a, b, jump)
}

func (f *function) expressionToRegister(e exprDesc, r int) exprDesc {
	if e = f.dischargeToRegister(e, r); e.kind == kindJump {
		e.t = f.Concatenate(e.t, e.info)
	}
	if e.hasJumps() {
		loadFalse, loadTrue := noJump, noJump
		if f.needValue(e.t) || f.needValue(e.f) {
			jump := noJump
			if e.kind != kindJump {
				jump = f.Jump()
			}
			loadFalse, loadTrue = f.encodeLabel(r, 0, 1), f.encodeLabel(r, 1, 0)
			f.PatchToHere(jump)
		}
		end := f.Label()
		f.patchListHelper(e.f, end, r, loadFalse)
		f.patchListHelper(e.t, end, r, loadTrue)
	}
	e.f, e.t, e.info, e.kind = noJump, noJump, r, kindNonRelocatable
	return e
}

func (f *function) ExpressionToNextRegister(e exprDesc) exprDesc {
	e = f.DischargeVariables(e)
	f.freeExpression(e)
	f.ReserveRegisters(1)
	return f.expressionToRegister(e, f.freeRegisterCount-1)
}

func (f *function) ExpressionToAnyRegister(e exprDesc) exprDesc {
	if e = f.DischargeVariables(e); e.kind == kindNonRelocatable {
		if e.hasJumps() {
			return e
		}
		if e.info >= f.activeVariableCount {
			return f.expressionToRegister(e, e.info)
		}
	}
	return f.ExpressionToNextRegister(e)
}

func (f *function) ExpressionToValue(e exprDesc) exprDesc {
	if e.hasJumps() {
		return f.ExpressionToAnyRegister(e)
	}
	return f.DischargeVariables(e)
}

func (f *function) ExpressionToRegisterOrConstant(e exprDesc) (exprDesc, int) {
	switch e = f.ExpressionToValue(e); e.kind {
	case kindTrue, kindFalse:
		if len(f.constants) <= maxIndexRK {
			e.info, e.kind = f.booleanConstant(e.kind == kindTrue), kindConstant
			return e, asConstant(e.info)
		}
	case kindNil:
		if len(f.constants) <= maxIndexRK {
			e.info, e.kind = f.nilConstant(), kindConstant
			return e, asConstant(e.info)
		}
	case kindNumber:
		e.info, e.kind = f.NumberConstant(e.value), kindConstant
		fallthrough
	case kindConstant:
		if e.info <= maxIndexRK {
			return e, asConstant(e.info)
		}
	}
	e = f.ExpressionToAnyRegister(e)
	return e, e.info
}

func (f *function) StoreVariable(v, e exprDesc) {
	switch v.kind {
	case kindLocal:
		f.freeExpression(e)
		f.expressionToRegister(e, v.info)
		return
	case kindUpValue:
		e = f.ExpressionToAnyRegister(e)
		f.EncodeABC(opSetUpValue, e.info, v.info, 0)
	case kindIndexed:
		var r int
		e, r = f.ExpressionToRegisterOrConstant(e)
		if v.tableType == kindLocal {
			f.EncodeABC(opSetTable, v.table, v.index, r)
		} else {
			f.EncodeABC(opSetTableUp, v.table, v.index, r)
		}
	default:
		f.unreachable()
	}
	f.freeExpression(e)
}

func (f *function) Self(e, key exprDesc) exprDesc {
	e = f.ExpressionToAnyRegister(e)
	r := e.info
	f.freeExpression(e)
	result := exprDesc{info: f.freeRegisterCount, kind: kindNonRelocatable} // base register for opSelf
	f.ReserveRegisters(2)                                                   // function and 'self' produced by opSelf
	key, k := f.ExpressionToRegisterOrConstant(key)
	f.EncodeABC(opSelf, result.info, r, k)
	f.freeExpression(key)
	return result
}

func (f *function) invertJump(pc int) {
	i := f.jumpControl(pc)
	f.p.l.assert(testTMode(i.opCode()) && i.opCode() != opTestSet && i.opCode() != opTest)
	i.setA(not(i.a()))
}

func (f *function) jumpOnCondition(e exprDesc, cond int) int {
	if e.kind == kindRelocatable {
		if i := f.instruction(e); i.opCode() == opNot {
			f.pc-- // remove previous opNot
			return f.conditionalJump(opTest, i.b(), 0, not(cond))
		}
	}
	f.dischargeToAnyRegister(e)
	f.freeExpression(e)
	return f.conditionalJump(opTestSet, noRegister, e.info, cond)
}

func (f *function) GoIfTrue(e exprDesc) exprDesc {
	pc := noJump
	switch e = f.DischargeVariables(e); e.kind {
	case kindJump:
		f.invertJump(e.info)
		pc = e.info
	case kindConstant, kindNumber, kindTrue:
	default:
		pc = f.jumpOnCondition(e, 0)
	}
	e.f = f.Concatenate(e.f, pc)
	f.PatchToHere(e.t)
	e.t = noJump
	return e
}

func (f *function) GoIfFalse(e exprDesc) exprDesc {
	pc := noJump
	switch e = f.DischargeVariables(e); e.kind {
	case kindJump:
		pc = e.info
	case kindNil, kindFalse:
	default:
		pc = f.jumpOnCondition(e, 1)
	}
	e.t = f.Concatenate(e.t, pc)
	f.PatchToHere(e.f)
	e.f = noJump
	return e
}

func (f *function) encodeNot(e exprDesc) exprDesc {
	switch e = f.DischargeVariables(e); e.kind {
	case kindNil, kindFalse:
		e.kind = kindTrue
	case kindConstant, kindNumber, kindTrue:
		e.kind = kindFalse
	case kindJump:
		f.invertJump(e.info)
	case kindRelocatable, kindNonRelocatable:
		e = f.dischargeToAnyRegister(e)
		f.freeExpression(e)
		e.info, e.kind = f.EncodeABC(opNot, 0, e.info, 0), kindRelocatable
	default:
		f.unreachable()
	}
	e.f, e.t = e.t, e.f
	f.removeValues(e.f)
	f.removeValues(e.t)
	return e
}

func (f *function) Indexed(t, k exprDesc) (r exprDesc) {
	f.assert(!t.hasJumps())
	r.table = t.info
	k, r.index = f.ExpressionToRegisterOrConstant(k)
	if t.kind == kindUpValue {
		r.tableType = kindUpValue
	} else {
		f.assert(t.kind == kindNonRelocatable || t.kind == kindLocal)
		r.tableType = kindLocal
	}
	r.kind = kindIndexed
	return
}

func foldConstants(op opCode, e1, e2 exprDesc) (exprDesc, bool) {
	if !e1.isNumeral() || !e2.isNumeral() {
		return e1, false
	} else if (op == opDiv || op == opMod) && e2.value == 0.0 {
		return e1, false
	}
	e1.value = arith(int(op-opAdd)+OpAdd, e1.value, e2.value)
	return e1, true
}

func (f *function) encodeArithmetic(op opCode, e1, e2 exprDesc, line int) exprDesc {
	if e, folded := foldConstants(op, e1, e2); folded {
		return e
	}
	o2 := 0
	if op != opUnaryMinus && op != opLength {
		e2, o2 = f.ExpressionToRegisterOrConstant(e2)
	}
	e1, o1 := f.ExpressionToRegisterOrConstant(e1)
	if o1 > o2 {
		f.freeExpression(e1)
		f.freeExpression(e2)
	} else {
		f.freeExpression(e2)
		f.freeExpression(e1)
	}
	e1.info, e1.kind = f.EncodeABC(op, 0, o1, o2), kindRelocatable
	f.FixLine(line)
	return e1
}

func (f *function) Prefix(op int, e exprDesc, line int) exprDesc {
	switch op {
	case oprMinus:
		if e.isNumeral() {
			e.value = -e.value
			return e
		} else {
			return f.encodeArithmetic(opUnaryMinus, f.ExpressionToAnyRegister(e), makeExpression(kindNumber, 0), line)
		}
	case oprNot:
		return f.encodeNot(e)
	case oprLength:
		return f.encodeArithmetic(opLength, f.ExpressionToAnyRegister(e), makeExpression(kindNumber, 0), line)
	}
	panic("unreachable")
}

func (f *function) Infix(op int, e exprDesc) exprDesc {
	switch op {
	case oprAnd:
		e = f.GoIfTrue(e)
	case oprOr:
		e = f.GoIfFalse(e)
	case oprConcat:
		e = f.ExpressionToNextRegister(e)
	case oprAdd, oprSub, oprMul, oprDiv, oprMod, oprPow:
		if !e.isNumeral() {
			e, _ = f.ExpressionToRegisterOrConstant(e)
		}
	default:
		e, _ = f.ExpressionToRegisterOrConstant(e)
	}
	return e
}

func (f *function) encodeComparison(op opCode, cond int, e1, e2 exprDesc) exprDesc {
	e1, o1 := f.ExpressionToRegisterOrConstant(e1)
	e2, o2 := f.ExpressionToRegisterOrConstant(e2)
	f.freeExpression(e2)
	f.freeExpression(e1)
	if cond == 0 && op != opEqual {
		o1, o2, cond = o2, o1, 1
	}
	return makeExpression(kindJump, f.conditionalJump(op, cond, o1, o2))
}

func (f *function) Postfix(op int, e1, e2 exprDesc, line int) exprDesc {
	switch op {
	case oprAnd:
		f.assert(e1.t == noJump)
		e2 = f.DischargeVariables(e2)
		e2.f = f.Concatenate(e2.f, e1.f)
		return e2
	case oprOr:
		f.assert(e1.f == noJump)
		e2 = f.DischargeVariables(e2)
		e2.t = f.Concatenate(e2.t, e1.t)
		return e2
	case oprConcat:
	case oprAdd, oprSub, oprMul, oprDiv, oprMod, oprPow:
		return f.encodeArithmetic(opCode(op-oprAdd)+opAdd, e1, e2, line)
	case oprEq, oprLT, oprLE:
		return f.encodeComparison(opCode(op-oprEq)+opEqual, 1, e1, e2)
	case oprNE, oprGT, oprGE:
		return f.encodeComparison(opCode(op-oprNE)+opEqual, 0, e1, e2)
	}
	panic("unreachable")
}

func (f *function) FixLine(line int) {
	f.lines[f.pc-1] = line
}

func (f *function) SetList(base, elementCount, storeCount int) {
	if f.assert(storeCount != 0); storeCount == MultipleReturns {
		storeCount = 0
	}
	if c := (elementCount-1)/listItemsPerFlush + 1; c <= maxArgC {
		f.EncodeABC(opSetList, base, storeCount, c)
	} else if c <= maxArgAx {
		f.EncodeABC(opSetList, base, storeCount, 0)
		f.encodeExtraArg(c)
	} else {
		f.p.syntaxError("constructor too long")
	}
	f.freeRegisterCount = base + 1
}