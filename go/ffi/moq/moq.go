package moq

// #include <moq.h>
import "C"

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"reflect"
	"runtime"
	"runtime/cgo"
	"sync/atomic"
	"unsafe"
)

// This is needed, because as of go 1.24
// type RustBuffer C.RustBuffer cannot have methods,
// RustBuffer is treated as non-local type
type GoRustBuffer struct {
	inner C.RustBuffer
}

type RustBufferI interface {
	AsReader() *bytes.Reader
	Free()
	ToGoBytes() []byte
	Data() unsafe.Pointer
	Len() uint64
	Capacity() uint64
}

// C.RustBuffer fields exposed as an interface so they can be accessed in different Go packages.
// See https://github.com/golang/go/issues/13467
type ExternalCRustBuffer interface {
	Data() unsafe.Pointer
	Len() uint64
	Capacity() uint64
}

func RustBufferFromC(b C.RustBuffer) ExternalCRustBuffer {
	return GoRustBuffer{
		inner: b,
	}
}

func CFromRustBuffer(b ExternalCRustBuffer) C.RustBuffer {
	return C.RustBuffer{
		capacity: C.uint64_t(b.Capacity()),
		len:      C.uint64_t(b.Len()),
		data:     (*C.uchar)(b.Data()),
	}
}

func RustBufferFromExternal(b ExternalCRustBuffer) GoRustBuffer {
	return GoRustBuffer{
		inner: C.RustBuffer{
			capacity: C.uint64_t(b.Capacity()),
			len:      C.uint64_t(b.Len()),
			data:     (*C.uchar)(b.Data()),
		},
	}
}

func (cb GoRustBuffer) Capacity() uint64 {
	return uint64(cb.inner.capacity)
}

func (cb GoRustBuffer) Len() uint64 {
	return uint64(cb.inner.len)
}

func (cb GoRustBuffer) Data() unsafe.Pointer {
	return unsafe.Pointer(cb.inner.data)
}

func (cb GoRustBuffer) AsReader() *bytes.Reader {
	b := unsafe.Slice((*byte)(cb.inner.data), C.uint64_t(cb.inner.len))
	return bytes.NewReader(b)
}

func (cb GoRustBuffer) Free() {
	rustCall(func(status *C.RustCallStatus) bool {
		C.ffi_moq_ffi_rustbuffer_free(cb.inner, status)
		return false
	})
}

func (cb GoRustBuffer) ToGoBytes() []byte {
	return C.GoBytes(unsafe.Pointer(cb.inner.data), C.int(cb.inner.len))
}

func stringToRustBuffer(str string) C.RustBuffer {
	return bytesToRustBuffer([]byte(str))
}

func bytesToRustBuffer(b []byte) C.RustBuffer {
	if len(b) == 0 {
		return C.RustBuffer{}
	}
	// We can pass the pointer along here, as it is pinned
	// for the duration of this call
	foreign := C.ForeignBytes{
		len:  C.int(len(b)),
		data: (*C.uchar)(unsafe.Pointer(&b[0])),
	}

	return rustCall(func(status *C.RustCallStatus) C.RustBuffer {
		return C.ffi_moq_ffi_rustbuffer_from_bytes(foreign, status)
	})
}

type BufLifter[GoType any] interface {
	Lift(value RustBufferI) GoType
}

type BufLowerer[GoType any] interface {
	Lower(value GoType) C.RustBuffer
}

type BufReader[GoType any] interface {
	Read(reader io.Reader) GoType
}

type BufWriter[GoType any] interface {
	Write(writer io.Writer, value GoType)
}

func LowerIntoRustBuffer[GoType any](bufWriter BufWriter[GoType], value GoType) C.RustBuffer {
	// This might be not the most efficient way but it does not require knowing allocation size
	// beforehand
	var buffer bytes.Buffer
	bufWriter.Write(&buffer, value)

	bytes, err := io.ReadAll(&buffer)
	if err != nil {
		panic(fmt.Errorf("reading written data: %w", err))
	}
	return bytesToRustBuffer(bytes)
}

func LiftFromRustBuffer[GoType any](bufReader BufReader[GoType], rbuf RustBufferI) GoType {
	defer rbuf.Free()
	reader := rbuf.AsReader()
	item := bufReader.Read(reader)
	if reader.Len() > 0 {
		// TODO: Remove this
		leftover, _ := io.ReadAll(reader)
		panic(fmt.Errorf("Junk remaining in buffer after lifting: %s", string(leftover)))
	}
	return item
}

func rustCallWithError[E any, U any](converter BufReader[E], callback func(*C.RustCallStatus) U) (U, E) {
	var status C.RustCallStatus
	returnValue := callback(&status)
	err := checkCallStatus(converter, status)
	return returnValue, err
}

func checkCallStatus[E any](converter BufReader[E], status C.RustCallStatus) E {
	switch status.code {
	case 0:
		var zero E
		return zero
	case 1:
		return LiftFromRustBuffer(converter, GoRustBuffer{inner: status.errorBuf})
	case 2:
		// when the rust code sees a panic, it tries to construct a rustBuffer
		// with the message.  but if that code panics, then it just sends back
		// an empty buffer.
		if status.errorBuf.len > 0 {
			panic(fmt.Errorf("%s", FfiConverterStringINSTANCE.Lift(GoRustBuffer{inner: status.errorBuf})))
		} else {
			panic(fmt.Errorf("Rust panicked while handling Rust panic"))
		}
	default:
		panic(fmt.Errorf("unknown status code: %d", status.code))
	}
}

func checkCallStatusUnknown(status C.RustCallStatus) error {
	switch status.code {
	case 0:
		return nil
	case 1:
		panic(fmt.Errorf("function not returning an error returned an error"))
	case 2:
		// when the rust code sees a panic, it tries to construct a C.RustBuffer
		// with the message.  but if that code panics, then it just sends back
		// an empty buffer.
		if status.errorBuf.len > 0 {
			panic(fmt.Errorf("%s", FfiConverterStringINSTANCE.Lift(GoRustBuffer{
				inner: status.errorBuf,
			})))
		} else {
			panic(fmt.Errorf("Rust panicked while handling Rust panic"))
		}
	default:
		return fmt.Errorf("unknown status code: %d", status.code)
	}
}

func rustCall[U any](callback func(*C.RustCallStatus) U) U {
	returnValue, err := rustCallWithError[error](nil, callback)
	if err != nil {
		panic(err)
	}
	return returnValue
}

type NativeError interface {
	AsError() error
}

func writeInt8(writer io.Writer, value int8) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint8(writer io.Writer, value uint8) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt16(writer io.Writer, value int16) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint16(writer io.Writer, value uint16) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt32(writer io.Writer, value int32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint32(writer io.Writer, value uint32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt64(writer io.Writer, value int64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint64(writer io.Writer, value uint64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeFloat32(writer io.Writer, value float32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeFloat64(writer io.Writer, value float64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func readInt8(reader io.Reader) int8 {
	var result int8
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint8(reader io.Reader) uint8 {
	var result uint8
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt16(reader io.Reader) int16 {
	var result int16
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint16(reader io.Reader) uint16 {
	var result uint16
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt32(reader io.Reader) int32 {
	var result int32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint32(reader io.Reader) uint32 {
	var result uint32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt64(reader io.Reader) int64 {
	var result int64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint64(reader io.Reader) uint64 {
	var result uint64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readFloat32(reader io.Reader) float32 {
	var result float32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readFloat64(reader io.Reader) float64 {
	var result float64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func init() {

	uniffiCheckChecksums()
}

func uniffiCheckChecksums() {
	// Get the bindings contract version from our ComponentInterface
	bindingsContractVersion := 30
	// Get the scaffolding contract version by calling the into the dylib
	scaffoldingContractVersion := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint32_t {
		return C.ffi_moq_ffi_uniffi_contract_version()
	})
	if bindingsContractVersion != int(scaffoldingContractVersion) {
		// If this happens try cleaning and rebuilding your project
		panic("moq: UniFFI contract version mismatch")
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_func_moq_log_level()
		})
		if checksum != 24625 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_func_moq_log_level: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqaudioconsumer_cancel()
		})
		if checksum != 62285 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqaudioconsumer_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqaudioconsumer_next()
		})
		if checksum != 5941 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqaudioconsumer_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqaudioproducer_demand()
		})
		if checksum != 42822 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqaudioproducer_demand: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqaudioproducer_finish()
		})
		if checksum != 6287 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqaudioproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqaudioproducer_name()
		})
		if checksum != 63111 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqaudioproducer_name: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqaudioproducer_reservation()
		})
		if checksum != 43848 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqaudioproducer_reservation: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqaudioproducer_reset_epoch()
		})
		if checksum != 57448 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqaudioproducer_reset_epoch: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqaudioproducer_unused()
		})
		if checksum != 19225 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqaudioproducer_unused: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqaudioproducer_used()
		})
		if checksum != 63466 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqaudioproducer_used: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqaudioproducer_write()
		})
		if checksum != 22094 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqaudioproducer_write: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbandwidth_reserve()
		})
		if checksum != 60458 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbandwidth_reserve: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqreservation_grant()
		})
		if checksum != 59401 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqreservation_grant: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqreservation_update()
		})
		if checksum != 9626 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqreservation_update: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbinarysnapshotproducer_finish()
		})
		if checksum != 10338 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbinarysnapshotproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbinarysnapshotproducer_update()
		})
		if checksum != 56077 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbinarysnapshotproducer_update: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbinarystreamproducer_append()
		})
		if checksum != 1645 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbinarystreamproducer_append: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbinarystreamproducer_finish()
		})
		if checksum != 60630 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbinarystreamproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_decode_audio()
		})
		if checksum != 18081 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_decode_audio: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_fetch_group()
		})
		if checksum != 18633 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_fetch_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_fetch_media_group()
		})
		if checksum != 40237 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_fetch_media_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_resolve()
		})
		if checksum != 55875 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_resolve: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_catalog()
		})
		if checksum != 34722 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_catalog: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_media()
		})
		if checksum != 29917 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_media: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_track()
		})
		if checksum != 2348 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_track: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_json_snapshot()
		})
		if checksum != 46473 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_json_snapshot: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_json_stream()
		})
		if checksum != 3028 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_json_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_decode_video()
		})
		if checksum != 28752 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_decode_video: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcatalogconsumer_cancel()
		})
		if checksum != 65421 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcatalogconsumer_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcatalogconsumer_next()
		})
		if checksum != 33133 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcatalogconsumer_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgroupconsumer_cancel()
		})
		if checksum != 52548 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgroupconsumer_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgroupconsumer_read_frame()
		})
		if checksum != 26363 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgroupconsumer_read_frame: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgroupconsumer_sequence()
		})
		if checksum != 46527 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgroupconsumer_sequence: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaconsumer_cancel()
		})
		if checksum != 14280 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaconsumer_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaconsumer_next()
		})
		if checksum != 42389 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaconsumer_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediagroupconsumer_cancel()
		})
		if checksum != 47486 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediagroupconsumer_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediagroupconsumer_next()
		})
		if checksum != 22636 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediagroupconsumer_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediagroupconsumer_sequence()
		})
		if checksum != 22332 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediagroupconsumer_sequence: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackconsumer_cancel()
		})
		if checksum != 65022 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackconsumer_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackconsumer_info()
		})
		if checksum != 46426 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackconsumer_info: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackconsumer_next_group()
		})
		if checksum != 5449 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackconsumer_next_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackconsumer_read_frame()
		})
		if checksum != 42799 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackconsumer_read_frame: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackconsumer_recv_datagram()
		})
		if checksum != 17412 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackconsumer_recv_datagram: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackconsumer_recv_group()
		})
		if checksum != 60887 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackconsumer_recv_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackconsumer_update()
		})
		if checksum != 24851 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackconsumer_update: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackdemand_is_used()
		})
		if checksum != 62559 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackdemand_is_used: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackdemand_name()
		})
		if checksum != 14603 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackdemand_name: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackdemand_unused()
		})
		if checksum != 32953 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackdemand_unused: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackdemand_used()
		})
		if checksum != 18944 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackdemand_used: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonsnapshotconsumer_cancel()
		})
		if checksum != 45114 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonsnapshotconsumer_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonsnapshotconsumer_next()
		})
		if checksum != 64727 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonsnapshotconsumer_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonsnapshotproducer_demand()
		})
		if checksum != 45789 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonsnapshotproducer_demand: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonsnapshotproducer_finish()
		})
		if checksum != 42593 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonsnapshotproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonsnapshotproducer_update()
		})
		if checksum != 18037 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonsnapshotproducer_update: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonstreamconsumer_cancel()
		})
		if checksum != 29308 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonstreamconsumer_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonstreamconsumer_next()
		})
		if checksum != 7523 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonstreamconsumer_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonstreamproducer_append()
		})
		if checksum != 12571 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonstreamproducer_append: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonstreamproducer_demand()
		})
		if checksum != 52854 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonstreamproducer_demand: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonstreamproducer_finish()
		})
		if checksum != 51459 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonstreamproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqannounceconsumer_cancel()
		})
		if checksum != 10799 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqannounceconsumer_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqannounceconsumer_next()
		})
		if checksum != 4892 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqannounceconsumer_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqannounceupdate_active()
		})
		if checksum != 49521 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqannounceupdate_active: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqannounceupdate_captures()
		})
		if checksum != 53535 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqannounceupdate_captures: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqannounceupdate_prefix()
		})
		if checksum != 8170 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqannounceupdate_prefix: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqannounceupdate_route()
		})
		if checksum != 8074 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqannounceupdate_route: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqannouncedbroadcast_available()
		})
		if checksum != 37458 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqannouncedbroadcast_available: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqannouncedbroadcast_cancel()
		})
		if checksum != 63175 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqannouncedbroadcast_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastrequest_accept()
		})
		if checksum != 36946 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastrequest_accept: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastrequest_path()
		})
		if checksum != 6534 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastrequest_path: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastrequest_reject()
		})
		if checksum != 9727 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastrequest_reject: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqoriginconsumer_announced()
		})
		if checksum != 16595 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqoriginconsumer_announced: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqoriginconsumer_announced_broadcast()
		})
		if checksum != 8509 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqoriginconsumer_announced_broadcast: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqoriginconsumer_request_broadcast()
		})
		if checksum != 64026 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqoriginconsumer_request_broadcast: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqorigindynamic_cancel()
		})
		if checksum != 47453 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqorigindynamic_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqorigindynamic_requested_broadcast()
		})
		if checksum != 54021 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqorigindynamic_requested_broadcast: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqorigindynamic_update()
		})
		if checksum != 27700 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqorigindynamic_update: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqoriginproducer_consume()
		})
		if checksum != 52357 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqoriginproducer_consume: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqoriginproducer_create_broadcast()
		})
		if checksum != 48971 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqoriginproducer_create_broadcast: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqoriginproducer_dynamic()
		})
		if checksum != 56233 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqoriginproducer_dynamic: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastdynamic_cancel()
		})
		if checksum != 25875 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastdynamic_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastdynamic_requested_track()
		})
		if checksum != 24118 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastdynamic_requested_track: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_encode_audio()
		})
		if checksum != 25334 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_encode_audio: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_binary_snapshot()
		})
		if checksum != 6748 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_binary_snapshot: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_binary_stream()
		})
		if checksum != 58418 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_binary_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_json_snapshot()
		})
		if checksum != 64276 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_json_snapshot: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_json_stream()
		})
		if checksum != 54975 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_json_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_announce()
		})
		if checksum != 13700 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_announce: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_close()
		})
		if checksum != 19191 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_close: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_consume()
		})
		if checksum != 27634 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_consume: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_dynamic()
		})
		if checksum != 55635 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_dynamic: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_finish()
		})
		if checksum != 29562 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_audio()
		})
		if checksum != 31691 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_audio: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_audio_on_track()
		})
		if checksum != 33897 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_audio_on_track: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_cmaf()
		})
		if checksum != 55701 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_cmaf: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_container()
		})
		if checksum != 24539 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_container: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_container_stream()
		})
		if checksum != 11217 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_container_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_track()
		})
		if checksum != 44452 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_track: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_video()
		})
		if checksum != 16383 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_video: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_video_on_track()
		})
		if checksum != 60666 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_video_on_track: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_video_stream()
		})
		if checksum != 28640 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_video_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_remove_catalog_section()
		})
		if checksum != 8608 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_remove_catalog_section: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_set_catalog_section()
		})
		if checksum != 28423 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_set_catalog_section: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_set_video_properties()
		})
		if checksum != 9178 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_set_video_properties: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_unannounce()
		})
		if checksum != 49647 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_unannounce: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_encode_video()
		})
		if checksum != 49251 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_encode_video: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcmafgroupproducer_abort()
		})
		if checksum != 15961 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcmafgroupproducer_abort: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcmafgroupproducer_finish()
		})
		if checksum != 60411 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcmafgroupproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcmafgroupproducer_sequence()
		})
		if checksum != 27137 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcmafgroupproducer_sequence: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcmafgroupproducer_write_fragment()
		})
		if checksum != 7411 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcmafgroupproducer_write_fragment: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcmafproducer_abort()
		})
		if checksum != 45318 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcmafproducer_abort: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcmafproducer_append_group()
		})
		if checksum != 35023 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcmafproducer_append_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcmafproducer_create_group()
		})
		if checksum != 52345 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcmafproducer_create_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcmafproducer_finish()
		})
		if checksum != 54852 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcmafproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcontainerproducer_cut()
		})
		if checksum != 17534 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcontainerproducer_cut: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcontainerproducer_finish()
		})
		if checksum != 13064 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcontainerproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcontainerproducer_seek()
		})
		if checksum != 61349 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcontainerproducer_seek: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcontainerproducer_write()
		})
		if checksum != 13274 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcontainerproducer_write: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcontainerstreamproducer_finish()
		})
		if checksum != 29733 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcontainerstreamproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcontainerstreamproducer_write()
		})
		if checksum != 18446 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcontainerstreamproducer_write: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgroupproducer_abort()
		})
		if checksum != 59787 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgroupproducer_abort: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgroupproducer_consume()
		})
		if checksum != 53274 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgroupproducer_consume: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgroupproducer_finish()
		})
		if checksum != 61241 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgroupproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgroupproducer_sequence()
		})
		if checksum != 21067 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgroupproducer_sequence: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgroupproducer_write_frame()
		})
		if checksum != 2442 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgroupproducer_write_frame: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgrouprequest_abort()
		})
		if checksum != 26970 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgrouprequest_abort: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgrouprequest_accept()
		})
		if checksum != 48242 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgrouprequest_accept: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgrouprequest_priority()
		})
		if checksum != 1745 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgrouprequest_priority: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgrouprequest_sequence()
		})
		if checksum != 29523 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgrouprequest_sequence: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaproducer_cut()
		})
		if checksum != 58543 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaproducer_cut: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaproducer_demand()
		})
		if checksum != 44491 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaproducer_demand: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaproducer_discontinuity()
		})
		if checksum != 35894 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaproducer_discontinuity: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaproducer_finish()
		})
		if checksum != 38480 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaproducer_flush()
		})
		if checksum != 10235 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaproducer_flush: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaproducer_name()
		})
		if checksum != 7199 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaproducer_name: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaproducer_seek()
		})
		if checksum != 43157 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaproducer_seek: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaproducer_unused()
		})
		if checksum != 38139 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaproducer_unused: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaproducer_used()
		})
		if checksum != 55925 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaproducer_used: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaproducer_write_frame()
		})
		if checksum != 7321 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaproducer_write_frame: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediastreamproducer_finish()
		})
		if checksum != 2732 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediastreamproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediastreamproducer_write()
		})
		if checksum != 31109 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediastreamproducer_write: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackdynamic_cancel()
		})
		if checksum != 57913 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackdynamic_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackdynamic_requested_group()
		})
		if checksum != 63983 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackdynamic_requested_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_abort()
		})
		if checksum != 37537 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_abort: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_append_datagram()
		})
		if checksum != 6272 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_append_datagram: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_append_group()
		})
		if checksum != 45225 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_append_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_consume()
		})
		if checksum != 30970 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_consume: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_create_group()
		})
		if checksum != 38978 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_create_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_demand()
		})
		if checksum != 32311 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_demand: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_dynamic()
		})
		if checksum != 58584 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_dynamic: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_finish()
		})
		if checksum != 3278 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_finish_at()
		})
		if checksum != 24581 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_finish_at: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_name()
		})
		if checksum != 14598 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_name: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_unused()
		})
		if checksum != 29609 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_unused: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_used()
		})
		if checksum != 19906 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_used: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_write_frame()
		})
		if checksum != 18663 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_write_frame: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackrequest_abort()
		})
		if checksum != 62713 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackrequest_abort: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackrequest_accept()
		})
		if checksum != 47766 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackrequest_accept: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackrequest_dynamic()
		})
		if checksum != 24801 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackrequest_dynamic: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackrequest_name()
		})
		if checksum != 56715 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackrequest_name: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqrequest_accept()
		})
		if checksum != 46183 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqrequest_accept: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqrequest_cancel()
		})
		if checksum != 25859 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqrequest_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqrequest_path()
		})
		if checksum != 48052 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqrequest_path: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqrequest_query()
		})
		if checksum != 23842 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqrequest_query: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqrequest_reject()
		})
		if checksum != 2829 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqrequest_reject: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqrequest_set_consume()
		})
		if checksum != 45399 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqrequest_set_consume: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqrequest_set_publish()
		})
		if checksum != 10746 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqrequest_set_publish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqrequest_transport()
		})
		if checksum != 57171 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqrequest_transport: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqrequest_url()
		})
		if checksum != 34138 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqrequest_url: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_accept()
		})
		if checksum != 44310 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_accept: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_cancel()
		})
		if checksum != 56970 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_cert_fingerprints()
		})
		if checksum != 32082 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_cert_fingerprints: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_listen()
		})
		if checksum != 9040 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_listen: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_set_bind()
		})
		if checksum != 55505 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_set_bind: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_set_consume()
		})
		if checksum != 13635 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_set_consume: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_set_publish()
		})
		if checksum != 48695 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_set_publish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_set_tls_cert()
		})
		if checksum != 33276 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_set_tls_cert: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_set_tls_generate()
		})
		if checksum != 148 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_set_tls_generate: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_set_tls_key()
		})
		if checksum != 56395 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_set_tls_key: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_cancel()
		})
		if checksum != 29949 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_connect()
		})
		if checksum != 42368 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_connect: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_backoff()
		})
		if checksum != 63523 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_backoff: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_bind()
		})
		if checksum != 56346 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_bind: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_consume()
		})
		if checksum != 4978 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_consume: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_publish()
		})
		if checksum != 64932 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_publish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_quic_max_streams()
		})
		if checksum != 17062 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_quic_max_streams: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_reconnect()
		})
		if checksum != 53736 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_reconnect: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_tls_cert()
		})
		if checksum != 12773 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_tls_cert: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_tls_fingerprints()
		})
		if checksum != 50038 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_tls_fingerprints: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_tls_key()
		})
		if checksum != 19390 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_tls_key: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_tls_roots()
		})
		if checksum != 5399 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_tls_roots: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_tls_system_roots()
		})
		if checksum != 10239 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_tls_system_roots: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_tls_verify()
		})
		if checksum != 64525 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_tls_verify: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_websocket_delay()
		})
		if checksum != 53033 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_websocket_delay: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_websocket_enabled()
		})
		if checksum != 65261 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_websocket_enabled: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqsession_bandwidth()
		})
		if checksum != 8006 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqsession_bandwidth: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqsession_cancel()
		})
		if checksum != 39476 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqsession_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqsession_closed()
		})
		if checksum != 7901 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqsession_closed: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqsession_consume()
		})
		if checksum != 45358 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqsession_consume: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqsession_epoch()
		})
		if checksum != 32695 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqsession_epoch: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqsession_publish()
		})
		if checksum != 37960 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqsession_publish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqsession_shutdown()
		})
		if checksum != 820 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqsession_shutdown: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqsession_stats()
		})
		if checksum != 44305 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqsession_stats: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqsession_status()
		})
		if checksum != 49725 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqsession_status: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqvideoconsumer_cancel()
		})
		if checksum != 27071 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqvideoconsumer_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqvideoconsumer_next()
		})
		if checksum != 34191 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqvideoconsumer_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqvideoproducer_cut()
		})
		if checksum != 18974 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqvideoproducer_cut: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqvideoproducer_demand()
		})
		if checksum != 283 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqvideoproducer_demand: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqvideoproducer_finish()
		})
		if checksum != 59081 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqvideoproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqvideoproducer_name()
		})
		if checksum != 43551 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqvideoproducer_name: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqvideoproducer_reservation()
		})
		if checksum != 64430 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqvideoproducer_reservation: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqvideoproducer_set_bitrate()
		})
		if checksum != 28203 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqvideoproducer_set_bitrate: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqvideoproducer_unused()
		})
		if checksum != 49939 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqvideoproducer_unused: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqvideoproducer_used()
		})
		if checksum != 24872 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqvideoproducer_used: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqvideoproducer_write()
		})
		if checksum != 6141 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqvideoproducer_write: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_constructor_moqaudiocodec_aac()
		})
		if checksum != 18170 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_constructor_moqaudiocodec_aac: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_constructor_moqaudiocodec_opus()
		})
		if checksum != 64803 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_constructor_moqaudiocodec_opus: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_constructor_moqoriginproducer_new()
		})
		if checksum != 48126 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_constructor_moqoriginproducer_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_constructor_moqbroadcastproducer_new()
		})
		if checksum != 37572 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_constructor_moqbroadcastproducer_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_constructor_moqserver_new()
		})
		if checksum != 42979 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_constructor_moqserver_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_constructor_moqclient_new()
		})
		if checksum != 44907 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_constructor_moqclient_new: UniFFI API checksum mismatch")
		}
	}
}

type FfiConverterUint8 struct{}

var FfiConverterUint8INSTANCE = FfiConverterUint8{}

func (FfiConverterUint8) Lower(value uint8) C.uint8_t {
	return C.uint8_t(value)
}

func (FfiConverterUint8) Write(writer io.Writer, value uint8) {
	writeUint8(writer, value)
}

func (FfiConverterUint8) Lift(value C.uint8_t) uint8 {
	return uint8(value)
}

func (FfiConverterUint8) Read(reader io.Reader) uint8 {
	return readUint8(reader)
}

type FfiDestroyerUint8 struct{}

func (FfiDestroyerUint8) Destroy(_ uint8) {}

type FfiConverterUint16 struct{}

var FfiConverterUint16INSTANCE = FfiConverterUint16{}

func (FfiConverterUint16) Lower(value uint16) C.uint16_t {
	return C.uint16_t(value)
}

func (FfiConverterUint16) Write(writer io.Writer, value uint16) {
	writeUint16(writer, value)
}

func (FfiConverterUint16) Lift(value C.uint16_t) uint16 {
	return uint16(value)
}

func (FfiConverterUint16) Read(reader io.Reader) uint16 {
	return readUint16(reader)
}

type FfiDestroyerUint16 struct{}

func (FfiDestroyerUint16) Destroy(_ uint16) {}

type FfiConverterUint32 struct{}

var FfiConverterUint32INSTANCE = FfiConverterUint32{}

func (FfiConverterUint32) Lower(value uint32) C.uint32_t {
	return C.uint32_t(value)
}

func (FfiConverterUint32) Write(writer io.Writer, value uint32) {
	writeUint32(writer, value)
}

func (FfiConverterUint32) Lift(value C.uint32_t) uint32 {
	return uint32(value)
}

func (FfiConverterUint32) Read(reader io.Reader) uint32 {
	return readUint32(reader)
}

type FfiDestroyerUint32 struct{}

func (FfiDestroyerUint32) Destroy(_ uint32) {}

type FfiConverterUint64 struct{}

var FfiConverterUint64INSTANCE = FfiConverterUint64{}

func (FfiConverterUint64) Lower(value uint64) C.uint64_t {
	return C.uint64_t(value)
}

func (FfiConverterUint64) Write(writer io.Writer, value uint64) {
	writeUint64(writer, value)
}

func (FfiConverterUint64) Lift(value C.uint64_t) uint64 {
	return uint64(value)
}

func (FfiConverterUint64) Read(reader io.Reader) uint64 {
	return readUint64(reader)
}

type FfiDestroyerUint64 struct{}

func (FfiDestroyerUint64) Destroy(_ uint64) {}

type FfiConverterFloat64 struct{}

var FfiConverterFloat64INSTANCE = FfiConverterFloat64{}

func (FfiConverterFloat64) Lower(value float64) C.double {
	return C.double(value)
}

func (FfiConverterFloat64) Write(writer io.Writer, value float64) {
	writeFloat64(writer, value)
}

func (FfiConverterFloat64) Lift(value C.double) float64 {
	return float64(value)
}

func (FfiConverterFloat64) Read(reader io.Reader) float64 {
	return readFloat64(reader)
}

type FfiDestroyerFloat64 struct{}

func (FfiDestroyerFloat64) Destroy(_ float64) {}

type FfiConverterBool struct{}

var FfiConverterBoolINSTANCE = FfiConverterBool{}

func (FfiConverterBool) Lower(value bool) C.int8_t {
	if value {
		return C.int8_t(1)
	}
	return C.int8_t(0)
}

func (FfiConverterBool) Write(writer io.Writer, value bool) {
	if value {
		writeInt8(writer, 1)
	} else {
		writeInt8(writer, 0)
	}
}

func (FfiConverterBool) Lift(value C.int8_t) bool {
	return value != 0
}

func (FfiConverterBool) Read(reader io.Reader) bool {
	return readInt8(reader) != 0
}

type FfiDestroyerBool struct{}

func (FfiDestroyerBool) Destroy(_ bool) {}

type FfiConverterString struct{}

var FfiConverterStringINSTANCE = FfiConverterString{}

func (FfiConverterString) Lift(rb RustBufferI) string {
	defer rb.Free()
	reader := rb.AsReader()
	b, err := io.ReadAll(reader)
	if err != nil {
		panic(fmt.Errorf("reading reader: %w", err))
	}
	return string(b)
}

func (FfiConverterString) Read(reader io.Reader) string {
	length := readInt32(reader)
	buffer := make([]byte, length)
	read_length, err := reader.Read(buffer)
	if err != nil && err != io.EOF {
		panic(err)
	}
	if read_length != int(length) {
		panic(fmt.Errorf("bad read length when reading string, expected %d, read %d", length, read_length))
	}
	return string(buffer)
}

func (FfiConverterString) Lower(value string) C.RustBuffer {
	return stringToRustBuffer(value)
}

func (c FfiConverterString) LowerExternal(value string) ExternalCRustBuffer {
	return RustBufferFromC(stringToRustBuffer(value))
}

func (FfiConverterString) Write(writer io.Writer, value string) {
	if len(value) > math.MaxInt32 {
		panic("String is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	write_length, err := io.WriteString(writer, value)
	if err != nil {
		panic(err)
	}
	if write_length != len(value) {
		panic(fmt.Errorf("bad write length when writing string, expected %d, written %d", len(value), write_length))
	}
}

type FfiDestroyerString struct{}

func (FfiDestroyerString) Destroy(_ string) {}

type FfiConverterBytes struct{}

var FfiConverterBytesINSTANCE = FfiConverterBytes{}

func (c FfiConverterBytes) Lower(value []byte) C.RustBuffer {
	return LowerIntoRustBuffer[[]byte](c, value)
}

func (c FfiConverterBytes) LowerExternal(value []byte) ExternalCRustBuffer {
	return RustBufferFromC(c.Lower(value))
}

func (c FfiConverterBytes) Write(writer io.Writer, value []byte) {
	if len(value) > math.MaxInt32 {
		panic("[]byte is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	write_length, err := writer.Write(value)
	if err != nil {
		panic(err)
	}
	if write_length != len(value) {
		panic(fmt.Errorf("bad write length when writing []byte, expected %d, written %d", len(value), write_length))
	}
}

func (c FfiConverterBytes) Lift(rb RustBufferI) []byte {
	return LiftFromRustBuffer[[]byte](c, rb)
}

func (c FfiConverterBytes) Read(reader io.Reader) []byte {
	length := readInt32(reader)
	buffer := make([]byte, length)
	read_length, err := reader.Read(buffer)
	if err != nil && err != io.EOF {
		panic(err)
	}
	if read_length != int(length) {
		panic(fmt.Errorf("bad read length when reading []byte, expected %d, read %d", length, read_length))
	}
	return buffer
}

type FfiDestroyerBytes struct{}

func (FfiDestroyerBytes) Destroy(_ []byte) {}

// Below is an implementation of synchronization requirements outlined in the link.
// https://github.com/mozilla/uniffi-rs/blob/0dc031132d9493ca812c3af6e7dd60ad2ea95bf0/uniffi_bindgen/src/bindings/kotlin/templates/ObjectRuntime.kt#L31

type FfiObject struct {
	handle        C.uint64_t
	callCounter   atomic.Int64
	cloneFunction func(C.uint64_t, *C.RustCallStatus) C.uint64_t
	freeFunction  func(C.uint64_t, *C.RustCallStatus)
	destroyed     atomic.Bool
}

func newFfiObject(
	handle C.uint64_t,
	cloneFunction func(C.uint64_t, *C.RustCallStatus) C.uint64_t,
	freeFunction func(C.uint64_t, *C.RustCallStatus),
) FfiObject {
	return FfiObject{
		handle:        handle,
		cloneFunction: cloneFunction,
		freeFunction:  freeFunction,
	}
}

func (ffiObject *FfiObject) incrementPointer(debugName string) C.uint64_t {
	for {
		counter := ffiObject.callCounter.Load()
		if counter <= -1 {
			panic(fmt.Errorf("%v object has already been destroyed", debugName))
		}
		if counter == math.MaxInt64 {
			panic(fmt.Errorf("%v object call counter would overflow", debugName))
		}
		if ffiObject.callCounter.CompareAndSwap(counter, counter+1) {
			break
		}
	}

	return rustCall(func(status *C.RustCallStatus) C.uint64_t {
		return ffiObject.cloneFunction(ffiObject.handle, status)
	})
}

func (ffiObject *FfiObject) decrementPointer() {
	if ffiObject.callCounter.Add(-1) == -1 {
		ffiObject.freeRustArcPtr()
	}
}

func (ffiObject *FfiObject) destroy() {
	if ffiObject.destroyed.CompareAndSwap(false, true) {
		if ffiObject.callCounter.Add(-1) == -1 {
			ffiObject.freeRustArcPtr()
		}
	}
}

func (ffiObject *FfiObject) freeRustArcPtr() {
	if ffiObject.handle == 0 {
		return
	}
	rustCall(func(status *C.RustCallStatus) int32 {
		ffiObject.freeFunction(ffiObject.handle, status)
		return 0
	})
}

type MoqAnnounceConsumerInterface interface {
	// Cancel all current and future `next()` calls.
	//
	// Terminal: the announcement stream is released here, not when the handle is.
	Cancel()
	// Get the next route announcement or retraction. Returns `None` when the origin is closed.
	Next(
		ctx context.Context) (**MoqAnnounceUpdate, error)
}
type MoqAnnounceConsumer struct {
	ffiObject FfiObject
}

// Cancel all current and future `next()` calls.
//
// Terminal: the announcement stream is released here, not when the handle is.
func (_self *MoqAnnounceConsumer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqAnnounceConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqannounceconsumer_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Get the next route announcement or retraction. Returns `None` when the origin is closed.
func (_self *MoqAnnounceConsumer) Next(
	ctx context.Context) (**MoqAnnounceUpdate, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqAnnounceConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) **MoqAnnounceUpdate {
			return FfiConverterOptionalMoqAnnounceUpdateINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqannounceconsumer_next(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_rust_buffer(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	return res, err
}
func (object *MoqAnnounceConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqAnnounceConsumer struct{}

var FfiConverterMoqAnnounceConsumerINSTANCE = FfiConverterMoqAnnounceConsumer{}

func (c FfiConverterMoqAnnounceConsumer) Lift(handle C.uint64_t) *MoqAnnounceConsumer {
	result := &MoqAnnounceConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqannounceconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqannounceconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqAnnounceConsumer).Destroy)
	return result
}

func (c FfiConverterMoqAnnounceConsumer) Read(reader io.Reader) *MoqAnnounceConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqAnnounceConsumer) Lower(value *MoqAnnounceConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqAnnounceConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqAnnounceConsumer) Write(writer io.Writer, value *MoqAnnounceConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqAnnounceConsumer(handle uint64) *MoqAnnounceConsumer {
	return FfiConverterMoqAnnounceConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqAnnounceConsumer(value *MoqAnnounceConsumer) uint64 {
	return uint64(FfiConverterMoqAnnounceConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqAnnounceConsumer struct{}

func (_ FfiDestroyerMoqAnnounceConsumer) Destroy(value *MoqAnnounceConsumer) {
	value.Destroy()
}

// A route announcement (or retraction) from an origin.
//
// Carries no broadcast: resolve a specific path with
// `MoqOriginConsumer::request_broadcast` (after this update proves it is
// covered). Its prefix is relative to the origin. The application decides
// which paths name broadcasts.
type MoqAnnounceUpdateInterface interface {
	// Whether the route is active (`true`) or was retracted (`false`). A repeated
	// active announcement for the same prefix is a metadata update.
	Active() bool
	// What each wildcard matched, or `None` when the route only overlaps the scope.
	Captures() *[]string
	// The covered prefix, relative to the origin.
	Prefix() string
	// The route serving the prefix: its hops and costs.
	Route() MoqRoute
}

// A route announcement (or retraction) from an origin.
//
// Carries no broadcast: resolve a specific path with
// `MoqOriginConsumer::request_broadcast` (after this update proves it is
// covered). Its prefix is relative to the origin. The application decides
// which paths name broadcasts.
type MoqAnnounceUpdate struct {
	ffiObject FfiObject
}

// Whether the route is active (`true`) or was retracted (`false`). A repeated
// active announcement for the same prefix is a metadata update.
func (_self *MoqAnnounceUpdate) Active() bool {
	_pointer := _self.ffiObject.incrementPointer("*MoqAnnounceUpdate")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_moq_ffi_fn_method_moqannounceupdate_active(
			_pointer, _uniffiStatus)
	}))
}

// What each wildcard matched, or `None` when the route only overlaps the scope.
func (_self *MoqAnnounceUpdate) Captures() *[]string {
	_pointer := _self.ffiObject.incrementPointer("*MoqAnnounceUpdate")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalSequenceStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqannounceupdate_captures(
				_pointer, _uniffiStatus),
		}
	}))
}

// The covered prefix, relative to the origin.
func (_self *MoqAnnounceUpdate) Prefix() string {
	_pointer := _self.ffiObject.incrementPointer("*MoqAnnounceUpdate")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqannounceupdate_prefix(
				_pointer, _uniffiStatus),
		}
	}))
}

// The route serving the prefix: its hops and costs.
func (_self *MoqAnnounceUpdate) Route() MoqRoute {
	_pointer := _self.ffiObject.incrementPointer("*MoqAnnounceUpdate")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMoqRouteINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqannounceupdate_route(
				_pointer, _uniffiStatus),
		}
	}))
}
func (object *MoqAnnounceUpdate) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqAnnounceUpdate struct{}

var FfiConverterMoqAnnounceUpdateINSTANCE = FfiConverterMoqAnnounceUpdate{}

func (c FfiConverterMoqAnnounceUpdate) Lift(handle C.uint64_t) *MoqAnnounceUpdate {
	result := &MoqAnnounceUpdate{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqannounceupdate(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqannounceupdate(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqAnnounceUpdate).Destroy)
	return result
}

func (c FfiConverterMoqAnnounceUpdate) Read(reader io.Reader) *MoqAnnounceUpdate {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqAnnounceUpdate) Lower(value *MoqAnnounceUpdate) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqAnnounceUpdate")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqAnnounceUpdate) Write(writer io.Writer, value *MoqAnnounceUpdate) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqAnnounceUpdate(handle uint64) *MoqAnnounceUpdate {
	return FfiConverterMoqAnnounceUpdateINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqAnnounceUpdate(value *MoqAnnounceUpdate) uint64 {
	return uint64(FfiConverterMoqAnnounceUpdateINSTANCE.Lower(value))
}

type FfiDestroyerMoqAnnounceUpdate struct{}

func (_ FfiDestroyerMoqAnnounceUpdate) Destroy(value *MoqAnnounceUpdate) {
	value.Destroy()
}

// Waits for a specific broadcast to be announced.
type MoqAnnouncedBroadcastInterface interface {
	// Wait until the broadcast is announced. Returns `Closed` if cancelled or the origin is closed.
	//
	// Its end arrives as an inactive [`MoqAnnounceUpdate`] on the origin's announcements.
	Available(
		ctx context.Context) (*MoqBroadcastConsumer, error)
	// Cancel all current and future `available()` calls.
	//
	// Terminal: the announcement watch is released here, not when the handle is.
	Cancel()
}

// Waits for a specific broadcast to be announced.
type MoqAnnouncedBroadcast struct {
	ffiObject FfiObject
}

// Wait until the broadcast is announced. Returns `Closed` if cancelled or the origin is closed.
//
// Its end arrives as an inactive [`MoqAnnounceUpdate`] on the origin's announcements.
func (_self *MoqAnnouncedBroadcast) Available(
	ctx context.Context) (*MoqBroadcastConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqAnnouncedBroadcast")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqBroadcastConsumer {
			return FfiConverterMoqBroadcastConsumerINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqannouncedbroadcast_available(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_u64(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	return res, err
}

// Cancel all current and future `available()` calls.
//
// Terminal: the announcement watch is released here, not when the handle is.
func (_self *MoqAnnouncedBroadcast) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqAnnouncedBroadcast")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqannouncedbroadcast_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}
func (object *MoqAnnouncedBroadcast) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqAnnouncedBroadcast struct{}

var FfiConverterMoqAnnouncedBroadcastINSTANCE = FfiConverterMoqAnnouncedBroadcast{}

func (c FfiConverterMoqAnnouncedBroadcast) Lift(handle C.uint64_t) *MoqAnnouncedBroadcast {
	result := &MoqAnnouncedBroadcast{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqannouncedbroadcast(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqannouncedbroadcast(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqAnnouncedBroadcast).Destroy)
	return result
}

func (c FfiConverterMoqAnnouncedBroadcast) Read(reader io.Reader) *MoqAnnouncedBroadcast {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqAnnouncedBroadcast) Lower(value *MoqAnnouncedBroadcast) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqAnnouncedBroadcast")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqAnnouncedBroadcast) Write(writer io.Writer, value *MoqAnnouncedBroadcast) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqAnnouncedBroadcast(handle uint64) *MoqAnnouncedBroadcast {
	return FfiConverterMoqAnnouncedBroadcastINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqAnnouncedBroadcast(value *MoqAnnouncedBroadcast) uint64 {
	return uint64(FfiConverterMoqAnnouncedBroadcastINSTANCE.Lower(value))
}

type FfiDestroyerMoqAnnouncedBroadcast struct{}

func (_ FfiDestroyerMoqAnnouncedBroadcast) Destroy(value *MoqAnnouncedBroadcast) {
	value.Destroy()
}

// Audio codec selection for the encoder.
//
// An immutable object so adding a codec later does not break callers
// switching over a closed enum.
type MoqAudioCodecInterface interface {
}

// Audio codec selection for the encoder.
//
// An immutable object so adding a codec later does not break callers
// switching over a closed enum.
type MoqAudioCodec struct {
	ffiObject FfiObject
}

// AAC-LC (`mp4a.40.2`) through the platform's encoder, at the input's rate
// and layout. A host without one refuses it when the producer is built.
// Its frames are 1024 samples, so leave `frame_duration_us` at 0.
func MoqAudioCodecAac() *MoqAudioCodec {
	return FfiConverterMoqAudioCodecINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_constructor_moqaudiocodec_aac(_uniffiStatus)
	}))
}

// Opus (RFC 6716).
func MoqAudioCodecOpus() *MoqAudioCodec {
	return FfiConverterMoqAudioCodecINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_constructor_moqaudiocodec_opus(_uniffiStatus)
	}))
}

func (object *MoqAudioCodec) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqAudioCodec struct{}

var FfiConverterMoqAudioCodecINSTANCE = FfiConverterMoqAudioCodec{}

func (c FfiConverterMoqAudioCodec) Lift(handle C.uint64_t) *MoqAudioCodec {
	result := &MoqAudioCodec{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqaudiocodec(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqaudiocodec(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqAudioCodec).Destroy)
	return result
}

func (c FfiConverterMoqAudioCodec) Read(reader io.Reader) *MoqAudioCodec {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqAudioCodec) Lower(value *MoqAudioCodec) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqAudioCodec")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqAudioCodec) Write(writer io.Writer, value *MoqAudioCodec) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqAudioCodec(handle uint64) *MoqAudioCodec {
	return FfiConverterMoqAudioCodecINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqAudioCodec(value *MoqAudioCodec) uint64 {
	return uint64(FfiConverterMoqAudioCodecINSTANCE.Lower(value))
}

type FfiDestroyerMoqAudioCodec struct{}

func (_ FfiDestroyerMoqAudioCodec) Destroy(value *MoqAudioCodec) {
	value.Destroy()
}

// Consumer for a raw-audio track.
type MoqAudioConsumerInterface interface {
	// Make current and future reads return `Cancelled`.
	//
	// Terminal: the decoder session is released here, not when the handle is.
	Cancel()
	// The next decoded frame, or `None` once the track ends.
	Next(
		ctx context.Context) (*MoqAudioFrame, error)
}

// Consumer for a raw-audio track.
type MoqAudioConsumer struct {
	ffiObject FfiObject
}

// Make current and future reads return `Cancelled`.
//
// Terminal: the decoder session is released here, not when the handle is.
func (_self *MoqAudioConsumer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqAudioConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqaudioconsumer_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// The next decoded frame, or `None` once the track ends.
func (_self *MoqAudioConsumer) Next(
	ctx context.Context) (*MoqAudioFrame, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqAudioConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MoqAudioFrame {
			return FfiConverterOptionalMoqAudioFrameINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqaudioconsumer_next(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_rust_buffer(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	return res, err
}
func (object *MoqAudioConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqAudioConsumer struct{}

var FfiConverterMoqAudioConsumerINSTANCE = FfiConverterMoqAudioConsumer{}

func (c FfiConverterMoqAudioConsumer) Lift(handle C.uint64_t) *MoqAudioConsumer {
	result := &MoqAudioConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqaudioconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqaudioconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqAudioConsumer).Destroy)
	return result
}

func (c FfiConverterMoqAudioConsumer) Read(reader io.Reader) *MoqAudioConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqAudioConsumer) Lower(value *MoqAudioConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqAudioConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqAudioConsumer) Write(writer io.Writer, value *MoqAudioConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqAudioConsumer(handle uint64) *MoqAudioConsumer {
	return FfiConverterMoqAudioConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqAudioConsumer(value *MoqAudioConsumer) uint64 {
	return uint64(FfiConverterMoqAudioConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqAudioConsumer struct{}

func (_ FfiDestroyerMoqAudioConsumer) Destroy(value *MoqAudioConsumer) {
	value.Destroy()
}

// Producer for a raw-audio track.
//
// Built via [`MoqBroadcastProducer::publish_audio`]. Each
// [`write`](Self::write) accepts an [`MoqAudioFrame`] whose `data`
// is PCM in the format declared by the [`MoqAudioEncoderInput`]
// passed at publish time.
type MoqAudioProducerInterface interface {
	// A watch-only handle to whether this audio track has subscribers.
	Demand() (*MoqTrackDemand, error)
	Finish() error
	// Return the name of this audio track.
	Name() (string, error)
	// This encoder's bandwidth reservation, if it was published against a
	// [`MoqBandwidth`]. Dropping the handle does not release the claim; the
	// producer holds it until [`finish`](Self::finish).
	Reservation() **MoqReservation
	// Re-anchor the timeline to the next frame's timestamp.
	//
	// Call this before writing after an idle gap so the gap remains visible in
	// the audio PTS instead of being compressed out by the running sample count.
	ResetEpoch() error
	// Wait until this audio track has no active consumers.
	//
	// Prefer [`demand`](Self::demand), a handle that can wait without borrowing this producer.
	Unused(
		ctx context.Context) error
	// Wait until this audio track has at least one active consumer.
	//
	// Prefer [`demand`](Self::demand), a handle that can wait without borrowing this producer.
	Used(
		ctx context.Context) error
	Write(frame MoqAudioFrame) error
}

// Producer for a raw-audio track.
//
// Built via [`MoqBroadcastProducer::publish_audio`]. Each
// [`write`](Self::write) accepts an [`MoqAudioFrame`] whose `data`
// is PCM in the format declared by the [`MoqAudioEncoderInput`]
// passed at publish time.
type MoqAudioProducer struct {
	ffiObject FfiObject
}

// A watch-only handle to whether this audio track has subscribers.
func (_self *MoqAudioProducer) Demand() (*MoqTrackDemand, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqAudioProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqaudioproducer_demand(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqTrackDemand
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackDemandINSTANCE.Lift(_uniffiRV), nil
	}
}

func (_self *MoqAudioProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqAudioProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqaudioproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Return the name of this audio track.
func (_self *MoqAudioProducer) Name() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqAudioProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqaudioproducer_name(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// This encoder's bandwidth reservation, if it was published against a
// [`MoqBandwidth`]. Dropping the handle does not release the claim; the
// producer holds it until [`finish`](Self::finish).
func (_self *MoqAudioProducer) Reservation() **MoqReservation {
	_pointer := _self.ffiObject.incrementPointer("*MoqAudioProducer")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalMoqReservationINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqaudioproducer_reservation(
				_pointer, _uniffiStatus),
		}
	}))
}

// Re-anchor the timeline to the next frame's timestamp.
//
// Call this before writing after an idle gap so the gap remains visible in
// the audio PTS instead of being compressed out by the running sample count.
func (_self *MoqAudioProducer) ResetEpoch() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqAudioProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqaudioproducer_reset_epoch(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Wait until this audio track has no active consumers.
//
// Prefer [`demand`](Self::demand), a handle that can wait without borrowing this producer.
func (_self *MoqAudioProducer) Unused(
	ctx context.Context) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqAudioProducer")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqaudioproducer_unused(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_void(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	return err
}

// Wait until this audio track has at least one active consumer.
//
// Prefer [`demand`](Self::demand), a handle that can wait without borrowing this producer.
func (_self *MoqAudioProducer) Used(
	ctx context.Context) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqAudioProducer")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqaudioproducer_used(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_void(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	return err
}

func (_self *MoqAudioProducer) Write(frame MoqAudioFrame) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqAudioProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqaudioproducer_write(
			_pointer, FfiConverterMoqAudioFrameINSTANCE.Lower(frame), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqAudioProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqAudioProducer struct{}

var FfiConverterMoqAudioProducerINSTANCE = FfiConverterMoqAudioProducer{}

func (c FfiConverterMoqAudioProducer) Lift(handle C.uint64_t) *MoqAudioProducer {
	result := &MoqAudioProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqaudioproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqaudioproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqAudioProducer).Destroy)
	return result
}

func (c FfiConverterMoqAudioProducer) Read(reader io.Reader) *MoqAudioProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqAudioProducer) Lower(value *MoqAudioProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqAudioProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqAudioProducer) Write(writer io.Writer, value *MoqAudioProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqAudioProducer(handle uint64) *MoqAudioProducer {
	return FfiConverterMoqAudioProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqAudioProducer(value *MoqAudioProducer) uint64 {
	return uint64(FfiConverterMoqAudioProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqAudioProducer struct{}

func (_ FfiDestroyerMoqAudioProducer) Destroy(value *MoqAudioProducer) {
	value.Destroy()
}

// Divides one connection's send estimate among the tracks sharing it.
//
// Minted by [`MoqSession::bandwidth`](crate::session::MoqSession::bandwidth).
// Clones share one reservation registry, so two handles from the same session
// see each other's claims.
type MoqBandwidthInterface interface {
	// Reserve up to `max_bps` for `track`, returning the reservation.
	//
	// `max_bps` is a ceiling, not a measurement: reserve the most the track can
	// ever send. The reservation lasts as long as the returned handle; drop it
	// to hand the room back. [`MoqReservation::update`] moves the ceiling
	// without claiming twice.
	Reserve(track *MoqTrackProducer, maxBps uint64) (*MoqReservation, error)
}

// Divides one connection's send estimate among the tracks sharing it.
//
// Minted by [`MoqSession::bandwidth`](crate::session::MoqSession::bandwidth).
// Clones share one reservation registry, so two handles from the same session
// see each other's claims.
type MoqBandwidth struct {
	ffiObject FfiObject
}

// Reserve up to `max_bps` for `track`, returning the reservation.
//
// `max_bps` is a ceiling, not a measurement: reserve the most the track can
// ever send. The reservation lasts as long as the returned handle; drop it
// to hand the room back. [`MoqReservation::update`] moves the ceiling
// without claiming twice.
func (_self *MoqBandwidth) Reserve(track *MoqTrackProducer, maxBps uint64) (*MoqReservation, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBandwidth")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbandwidth_reserve(
			_pointer, FfiConverterMoqTrackProducerINSTANCE.Lower(track), FfiConverterUint64INSTANCE.Lower(maxBps), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqReservation
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqReservationINSTANCE.Lift(_uniffiRV), nil
	}
}
func (object *MoqBandwidth) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqBandwidth struct{}

var FfiConverterMoqBandwidthINSTANCE = FfiConverterMoqBandwidth{}

func (c FfiConverterMoqBandwidth) Lift(handle C.uint64_t) *MoqBandwidth {
	result := &MoqBandwidth{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqbandwidth(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqbandwidth(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqBandwidth).Destroy)
	return result
}

func (c FfiConverterMoqBandwidth) Read(reader io.Reader) *MoqBandwidth {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqBandwidth) Lower(value *MoqBandwidth) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqBandwidth")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqBandwidth) Write(writer io.Writer, value *MoqBandwidth) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqBandwidth(handle uint64) *MoqBandwidth {
	return FfiConverterMoqBandwidthINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqBandwidth(value *MoqBandwidth) uint64 {
	return uint64(FfiConverterMoqBandwidthINSTANCE.Lower(value))
}

type FfiDestroyerMoqBandwidth struct{}

func (_ FfiDestroyerMoqBandwidth) Destroy(value *MoqBandwidth) {
	value.Destroy()
}

// Publishes opaque payloads that consumers see as a single latest value.
type MoqBinarySnapshotProducerInterface interface {
	// Finish the track and retire its catalog entry.
	Finish() error
	// Publish a new payload, superseding the last.
	Update(payload []byte) error
}

// Publishes opaque payloads that consumers see as a single latest value.
type MoqBinarySnapshotProducer struct {
	ffiObject FfiObject
}

// Finish the track and retire its catalog entry.
func (_self *MoqBinarySnapshotProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBinarySnapshotProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbinarysnapshotproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Publish a new payload, superseding the last.
func (_self *MoqBinarySnapshotProducer) Update(payload []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBinarySnapshotProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbinarysnapshotproducer_update(
			_pointer, FfiConverterBytesINSTANCE.Lower(payload), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqBinarySnapshotProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqBinarySnapshotProducer struct{}

var FfiConverterMoqBinarySnapshotProducerINSTANCE = FfiConverterMoqBinarySnapshotProducer{}

func (c FfiConverterMoqBinarySnapshotProducer) Lift(handle C.uint64_t) *MoqBinarySnapshotProducer {
	result := &MoqBinarySnapshotProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqbinarysnapshotproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqbinarysnapshotproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqBinarySnapshotProducer).Destroy)
	return result
}

func (c FfiConverterMoqBinarySnapshotProducer) Read(reader io.Reader) *MoqBinarySnapshotProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqBinarySnapshotProducer) Lower(value *MoqBinarySnapshotProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqBinarySnapshotProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqBinarySnapshotProducer) Write(writer io.Writer, value *MoqBinarySnapshotProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqBinarySnapshotProducer(handle uint64) *MoqBinarySnapshotProducer {
	return FfiConverterMoqBinarySnapshotProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqBinarySnapshotProducer(value *MoqBinarySnapshotProducer) uint64 {
	return uint64(FfiConverterMoqBinarySnapshotProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqBinarySnapshotProducer struct{}

func (_ FfiDestroyerMoqBinarySnapshotProducer) Destroy(value *MoqBinarySnapshotProducer) {
	value.Destroy()
}

// Publishes an ordered log of opaque payloads, one per append.
type MoqBinaryStreamProducerInterface interface {
	// Append one payload to the log.
	Append(payload []byte) error
	// Finish the track and retire its catalog entry.
	Finish() error
}

// Publishes an ordered log of opaque payloads, one per append.
type MoqBinaryStreamProducer struct {
	ffiObject FfiObject
}

// Append one payload to the log.
func (_self *MoqBinaryStreamProducer) Append(payload []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBinaryStreamProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbinarystreamproducer_append(
			_pointer, FfiConverterBytesINSTANCE.Lower(payload), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Finish the track and retire its catalog entry.
func (_self *MoqBinaryStreamProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBinaryStreamProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbinarystreamproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqBinaryStreamProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqBinaryStreamProducer struct{}

var FfiConverterMoqBinaryStreamProducerINSTANCE = FfiConverterMoqBinaryStreamProducer{}

func (c FfiConverterMoqBinaryStreamProducer) Lift(handle C.uint64_t) *MoqBinaryStreamProducer {
	result := &MoqBinaryStreamProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqbinarystreamproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqbinarystreamproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqBinaryStreamProducer).Destroy)
	return result
}

func (c FfiConverterMoqBinaryStreamProducer) Read(reader io.Reader) *MoqBinaryStreamProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqBinaryStreamProducer) Lower(value *MoqBinaryStreamProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqBinaryStreamProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqBinaryStreamProducer) Write(writer io.Writer, value *MoqBinaryStreamProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqBinaryStreamProducer(handle uint64) *MoqBinaryStreamProducer {
	return FfiConverterMoqBinaryStreamProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqBinaryStreamProducer(value *MoqBinaryStreamProducer) uint64 {
	return uint64(FfiConverterMoqBinaryStreamProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqBinaryStreamProducer struct{}

func (_ FfiDestroyerMoqBinaryStreamProducer) Destroy(value *MoqBinaryStreamProducer) {
	value.Destroy()
}

type MoqBroadcastConsumerInterface interface {
	// Subscribe to an audio track. `catalog_audio_config` comes from
	// the catalog (see
	// [`MoqCatalogConsumer::next`](crate::consumer::MoqCatalogConsumer::next));
	// the codec is inferred from it. Only Opus and AAC-LC are supported.
	//
	// A rendition whose [`broadcast`](crate::media::MoqAudio::broadcast) names another broadcast
	// is subscribed there, so `name` is always read from the broadcast the catalog points at.
	DecodeAudio(
		ctx context.Context, name string, catalogAudio MoqAudio, output MoqAudioDecoderOutput) (*MoqAudioConsumer, error)
	// Fetch one complete group by track name and group sequence.
	//
	// This does not create a live subscription. A retained group resolves immediately;
	// otherwise the request waits for a dynamic producer to serve it. The returned
	// group may still be in progress, so read frames until `read_frame()` returns `None`.
	FetchGroup(
		ctx context.Context, name string, sequence uint64, options *MoqFetchGroupOptions) (*MoqGroupConsumer, error)
	// Fetch one group and decode its track container into media frames.
	//
	// Unlike [`Self::subscribe_media`], this does not create a live subscription or apply
	// age-based group skipping. The returned consumer reads exactly the requested group
	// until [`MoqMediaGroupConsumer::next`] returns `None`.
	FetchMediaGroup(
		ctx context.Context, name string, sequence uint64, container MoqContainer, options *MoqFetchGroupOptions) (*MoqMediaGroupConsumer, error)
	// Resolve a catalog rendition's `broadcast` reference to the broadcast serving its track.
	//
	// `reference` is [`MoqVideo::broadcast`] / [`MoqAudio::broadcast`]: absent or empty names
	// this broadcast, anything else names a sibling relative to it (e.g. `./source`). Call it on a
	// rendition that carries one before [`Self::subscribe_media`], [`Self::subscribe_track`],
	// [`Self::fetch_group`], or [`Self::fetch_media_group`], which take a track name rather than a
	// rendition; `decode_video` and `decode_audio` resolve it themselves.
	//
	// Errors if this broadcast came from a local producer rather than an origin, since a
	// standalone broadcast has no sibling to name, and reports a sibling that exists but is not
	// announced yet as unroutable rather than waiting for it (see
	// [`MoqOriginConsumer::request_broadcast`](crate::origin::MoqOriginConsumer::request_broadcast)).
	Resolve(
		ctx context.Context, reference *string) (*MoqBroadcastConsumer, error)
	// Subscribe to the catalog for this broadcast.
	SubscribeCatalog(
		ctx context.Context) (*MoqCatalogConsumer, error)
	// Subscribe to a track by name, delivering frames in decode order.
	//
	// `container` is the track container from the catalog.
	// `subscription` tunes delivery priority, group range, and staleness; omit for defaults.
	//
	// [`MoqSubscription::max_age_us`] bounds the local jitter buffer as well as
	// the publisher's cache, so both ends skip a stalled group on the same budget.
	SubscribeMedia(
		ctx context.Context, name string, container MoqContainer, subscription *MoqSubscription) (*MoqMediaConsumer, error)
	// Subscribe to a track by name, the same pattern as moq-boy's command/status tracks.
	//
	// Frames are returned as plain byte payloads with no codec or container parsing.
	// `subscription` tunes delivery priority, group range, and staleness; omit for defaults.
	SubscribeTrack(
		ctx context.Context, name string, subscription *MoqSubscription) (*MoqTrackConsumer, error)
	// Subscribe to a JSON snapshot track (lossy latest-value) by name.
	//
	// Pass the same [`MoqJsonSnapshotConfig::compression`] the producer used.
	SubscribeJsonSnapshot(
		ctx context.Context, name string, config MoqJsonSnapshotConfig) (*MoqJsonSnapshotConsumer, error)
	// Subscribe to a JSON stream track (lossless append-log) by name.
	SubscribeJsonStream(
		ctx context.Context, name string, config MoqJsonStreamConfig) (*MoqJsonStreamConsumer, error)
	// Subscribe to a video track and decode it inside the bindings.
	//
	// `catalog_video` comes from the catalog (see
	// [`MoqCatalogConsumer::next`](crate::consumer::MoqCatalogConsumer::next)); the codec is read
	// from it. Errors if no native backend handles that codec, rather than failing on the first
	// frame.
	//
	// A rendition whose [`broadcast`](crate::media::MoqVideo::broadcast) names another broadcast
	// is subscribed there, so `name` is always read from the broadcast the catalog points at.
	DecodeVideo(
		ctx context.Context, name string, catalogVideo MoqVideo, output MoqVideoDecoderOutput) (*MoqVideoConsumer, error)
}
type MoqBroadcastConsumer struct {
	ffiObject FfiObject
}

// Subscribe to an audio track. `catalog_audio_config` comes from
// the catalog (see
// [`MoqCatalogConsumer::next`](crate::consumer::MoqCatalogConsumer::next));
// the codec is inferred from it. Only Opus and AAC-LC are supported.
//
// A rendition whose [`broadcast`](crate::media::MoqAudio::broadcast) names another broadcast
// is subscribed there, so `name` is always read from the broadcast the catalog points at.
func (_self *MoqBroadcastConsumer) DecodeAudio(
	ctx context.Context, name string, catalogAudio MoqAudio, output MoqAudioDecoderOutput) (*MoqAudioConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqAudioConsumer {
			return FfiConverterMoqAudioConsumerINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_decode_audio(
				_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterMoqAudioINSTANCE.Lower(catalogAudio), FfiConverterMoqAudioDecoderOutputINSTANCE.Lower(output))
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_u64(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	return res, err
}

// Fetch one complete group by track name and group sequence.
//
// This does not create a live subscription. A retained group resolves immediately;
// otherwise the request waits for a dynamic producer to serve it. The returned
// group may still be in progress, so read frames until `read_frame()` returns `None`.
func (_self *MoqBroadcastConsumer) FetchGroup(
	ctx context.Context, name string, sequence uint64, options *MoqFetchGroupOptions) (*MoqGroupConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqGroupConsumer {
			return FfiConverterMoqGroupConsumerINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_fetch_group(
				_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterUint64INSTANCE.Lower(sequence), FfiConverterOptionalMoqFetchGroupOptionsINSTANCE.Lower(options))
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_u64(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	return res, err
}

// Fetch one group and decode its track container into media frames.
//
// Unlike [`Self::subscribe_media`], this does not create a live subscription or apply
// age-based group skipping. The returned consumer reads exactly the requested group
// until [`MoqMediaGroupConsumer::next`] returns `None`.
func (_self *MoqBroadcastConsumer) FetchMediaGroup(
	ctx context.Context, name string, sequence uint64, container MoqContainer, options *MoqFetchGroupOptions) (*MoqMediaGroupConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqMediaGroupConsumer {
			return FfiConverterMoqMediaGroupConsumerINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_fetch_media_group(
				_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterUint64INSTANCE.Lower(sequence), FfiConverterMoqContainerINSTANCE.Lower(container), FfiConverterOptionalMoqFetchGroupOptionsINSTANCE.Lower(options))
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_u64(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	return res, err
}

// Resolve a catalog rendition's `broadcast` reference to the broadcast serving its track.
//
// `reference` is [`MoqVideo::broadcast`] / [`MoqAudio::broadcast`]: absent or empty names
// this broadcast, anything else names a sibling relative to it (e.g. `./source`). Call it on a
// rendition that carries one before [`Self::subscribe_media`], [`Self::subscribe_track`],
// [`Self::fetch_group`], or [`Self::fetch_media_group`], which take a track name rather than a
// rendition; `decode_video` and `decode_audio` resolve it themselves.
//
// Errors if this broadcast came from a local producer rather than an origin, since a
// standalone broadcast has no sibling to name, and reports a sibling that exists but is not
// announced yet as unroutable rather than waiting for it (see
// [`MoqOriginConsumer::request_broadcast`](crate::origin::MoqOriginConsumer::request_broadcast)).
func (_self *MoqBroadcastConsumer) Resolve(
	ctx context.Context, reference *string) (*MoqBroadcastConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqBroadcastConsumer {
			return FfiConverterMoqBroadcastConsumerINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_resolve(
				_pointer, FfiConverterOptionalStringINSTANCE.Lower(reference))
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_u64(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	return res, err
}

// Subscribe to the catalog for this broadcast.
func (_self *MoqBroadcastConsumer) SubscribeCatalog(
	ctx context.Context) (*MoqCatalogConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqCatalogConsumer {
			return FfiConverterMoqCatalogConsumerINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_subscribe_catalog(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_u64(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	return res, err
}

// Subscribe to a track by name, delivering frames in decode order.
//
// `container` is the track container from the catalog.
// `subscription` tunes delivery priority, group range, and staleness; omit for defaults.
//
// [`MoqSubscription::max_age_us`] bounds the local jitter buffer as well as
// the publisher's cache, so both ends skip a stalled group on the same budget.
func (_self *MoqBroadcastConsumer) SubscribeMedia(
	ctx context.Context, name string, container MoqContainer, subscription *MoqSubscription) (*MoqMediaConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqMediaConsumer {
			return FfiConverterMoqMediaConsumerINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_subscribe_media(
				_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterMoqContainerINSTANCE.Lower(container), FfiConverterOptionalMoqSubscriptionINSTANCE.Lower(subscription))
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_u64(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	return res, err
}

// Subscribe to a track by name, the same pattern as moq-boy's command/status tracks.
//
// Frames are returned as plain byte payloads with no codec or container parsing.
// `subscription` tunes delivery priority, group range, and staleness; omit for defaults.
func (_self *MoqBroadcastConsumer) SubscribeTrack(
	ctx context.Context, name string, subscription *MoqSubscription) (*MoqTrackConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqTrackConsumer {
			return FfiConverterMoqTrackConsumerINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_subscribe_track(
				_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterOptionalMoqSubscriptionINSTANCE.Lower(subscription))
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_u64(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	return res, err
}

// Subscribe to a JSON snapshot track (lossy latest-value) by name.
//
// Pass the same [`MoqJsonSnapshotConfig::compression`] the producer used.
func (_self *MoqBroadcastConsumer) SubscribeJsonSnapshot(
	ctx context.Context, name string, config MoqJsonSnapshotConfig) (*MoqJsonSnapshotConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqJsonSnapshotConsumer {
			return FfiConverterMoqJsonSnapshotConsumerINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_subscribe_json_snapshot(
				_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterMoqJsonSnapshotConfigINSTANCE.Lower(config))
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_u64(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	return res, err
}

// Subscribe to a JSON stream track (lossless append-log) by name.
func (_self *MoqBroadcastConsumer) SubscribeJsonStream(
	ctx context.Context, name string, config MoqJsonStreamConfig) (*MoqJsonStreamConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqJsonStreamConsumer {
			return FfiConverterMoqJsonStreamConsumerINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_subscribe_json_stream(
				_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterMoqJsonStreamConfigINSTANCE.Lower(config))
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_u64(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	return res, err
}

// Subscribe to a video track and decode it inside the bindings.
//
// `catalog_video` comes from the catalog (see
// [`MoqCatalogConsumer::next`](crate::consumer::MoqCatalogConsumer::next)); the codec is read
// from it. Errors if no native backend handles that codec, rather than failing on the first
// frame.
//
// A rendition whose [`broadcast`](crate::media::MoqVideo::broadcast) names another broadcast
// is subscribed there, so `name` is always read from the broadcast the catalog points at.
func (_self *MoqBroadcastConsumer) DecodeVideo(
	ctx context.Context, name string, catalogVideo MoqVideo, output MoqVideoDecoderOutput) (*MoqVideoConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqVideoConsumer {
			return FfiConverterMoqVideoConsumerINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_decode_video(
				_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterMoqVideoINSTANCE.Lower(catalogVideo), FfiConverterMoqVideoDecoderOutputINSTANCE.Lower(output))
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_u64(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	return res, err
}
func (object *MoqBroadcastConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqBroadcastConsumer struct{}

var FfiConverterMoqBroadcastConsumerINSTANCE = FfiConverterMoqBroadcastConsumer{}

func (c FfiConverterMoqBroadcastConsumer) Lift(handle C.uint64_t) *MoqBroadcastConsumer {
	result := &MoqBroadcastConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqbroadcastconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqbroadcastconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqBroadcastConsumer).Destroy)
	return result
}

func (c FfiConverterMoqBroadcastConsumer) Read(reader io.Reader) *MoqBroadcastConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqBroadcastConsumer) Lower(value *MoqBroadcastConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqBroadcastConsumer) Write(writer io.Writer, value *MoqBroadcastConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqBroadcastConsumer(handle uint64) *MoqBroadcastConsumer {
	return FfiConverterMoqBroadcastConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqBroadcastConsumer(value *MoqBroadcastConsumer) uint64 {
	return uint64(FfiConverterMoqBroadcastConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqBroadcastConsumer struct{}

func (_ FfiDestroyerMoqBroadcastConsumer) Destroy(value *MoqBroadcastConsumer) {
	value.Destroy()
}

type MoqBroadcastDynamicInterface interface {
	// Cancel all current and future `requested_track()` calls.
	//
	// Terminal: the dynamic broadcast is released here, not when the handle is, so any pending
	// request is rejected.
	Cancel()
	// Wait for the next subscriber-requested track.
	//
	// Returns a [`MoqTrackRequest`]: accept it for raw writes with
	// [`MoqTrackRequest::accept`], publish media onto it with
	// [`MoqBroadcastProducer::publish_audio_on_track`], or reject it with
	// [`MoqTrackRequest::abort`]. The requesting subscriber stays pending until then.
	//
	// Returns an error once the broadcast is closed or aborted.
	RequestedTrack(
		ctx context.Context) (*MoqTrackRequest, error)
}
type MoqBroadcastDynamic struct {
	ffiObject FfiObject
}

// Cancel all current and future `requested_track()` calls.
//
// Terminal: the dynamic broadcast is released here, not when the handle is, so any pending
// request is rejected.
func (_self *MoqBroadcastDynamic) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastDynamic")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastdynamic_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Wait for the next subscriber-requested track.
//
// Returns a [`MoqTrackRequest`]: accept it for raw writes with
// [`MoqTrackRequest::accept`], publish media onto it with
// [`MoqBroadcastProducer::publish_audio_on_track`], or reject it with
// [`MoqTrackRequest::abort`]. The requesting subscriber stays pending until then.
//
// Returns an error once the broadcast is closed or aborted.
func (_self *MoqBroadcastDynamic) RequestedTrack(
	ctx context.Context) (*MoqTrackRequest, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastDynamic")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqTrackRequest {
			return FfiConverterMoqTrackRequestINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqbroadcastdynamic_requested_track(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_u64(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	return res, err
}
func (object *MoqBroadcastDynamic) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqBroadcastDynamic struct{}

var FfiConverterMoqBroadcastDynamicINSTANCE = FfiConverterMoqBroadcastDynamic{}

func (c FfiConverterMoqBroadcastDynamic) Lift(handle C.uint64_t) *MoqBroadcastDynamic {
	result := &MoqBroadcastDynamic{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqbroadcastdynamic(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqbroadcastdynamic(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqBroadcastDynamic).Destroy)
	return result
}

func (c FfiConverterMoqBroadcastDynamic) Read(reader io.Reader) *MoqBroadcastDynamic {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqBroadcastDynamic) Lower(value *MoqBroadcastDynamic) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqBroadcastDynamic")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqBroadcastDynamic) Write(writer io.Writer, value *MoqBroadcastDynamic) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqBroadcastDynamic(handle uint64) *MoqBroadcastDynamic {
	return FfiConverterMoqBroadcastDynamicINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqBroadcastDynamic(value *MoqBroadcastDynamic) uint64 {
	return uint64(FfiConverterMoqBroadcastDynamicINSTANCE.Lower(value))
}

type FfiDestroyerMoqBroadcastDynamic struct{}

func (_ FfiDestroyerMoqBroadcastDynamic) Destroy(value *MoqBroadcastDynamic) {
	value.Destroy()
}

type MoqBroadcastProducerInterface interface {
	// Open an audio track on this broadcast. The catalog rendition is
	// registered immediately so subscribers can find the track even
	// before the first frame is written.
	//
	// Pass `bandwidth` to reserve this track's bitrate against the session's
	// allocator. Following the grant waits on the Rust audio producer; this
	// call only claims the share so a co-resident video encoder sizes itself
	// against what is left.
	EncodeAudio(name string, input MoqAudioEncoderInput, output MoqAudioEncoderOutput, bandwidth **MoqBandwidth) (*MoqAudioProducer, error)
	// Publish a binary snapshot track (lossy latest-value) by name, advertised in the catalog.
	//
	// Errors if the catalog already carries an entry under `name`.
	PublishBinarySnapshot(name string, config MoqBinaryConfig) (*MoqBinarySnapshotProducer, error)
	// Publish a binary stream track (lossless append-log) by name, advertised in the catalog.
	//
	// Errors if the catalog already carries an entry under `name`.
	PublishBinaryStream(name string, config MoqBinaryConfig) (*MoqBinaryStreamProducer, error)
	// Publish a JSON snapshot track (lossy latest-value) by name, advertised in the catalog.
	//
	// The broadcast's catalog carries `json.tracks.<name>` (`mode: snapshot`, and
	// `compression: deflate` when set) for as long as the track lives; finishing or dropping the
	// producer retires it. Errors if the catalog already carries an entry under `name`.
	PublishJsonSnapshot(name string, config MoqJsonSnapshotConfig) (*MoqJsonSnapshotProducer, error)
	// Publish a JSON stream track (lossless append-log) by name, advertised in the catalog.
	//
	// The broadcast's catalog carries `json.tracks.<name>` (`mode: stream`) for as long as the
	// track lives. Errors if the catalog already carries an entry under `name`.
	PublishJsonStream(name string, config MoqJsonStreamConfig) (*MoqJsonStreamProducer, error)
	// Advertise this broadcast's exact path as a route.
	//
	// Until announced, the broadcast is invisible and unroutable for local
	// consumers and peers alike. Announcing again re-prices the route in place.
	// Errors with `Closed` on a standalone broadcast (no origin to announce on).
	Announce(route MoqRoute) error
	// End the broadcast for good: retract it, serve no new tracks, and finalize the catalog.
	//
	// Tracks already subscribed carry on to their own end. Every later call on this
	// producer fails with `Closed`; closing again is a no-op.
	Close() error
	// Create a consumer that reads from this broadcast's tracks.
	Consume() (*MoqBroadcastConsumer, error)
	// Create a dynamic producer that yields tracks requested by subscribers.
	//
	// Hold the returned object for as long as missing track requests should be
	// accepted. Dropping it makes future subscriptions to unknown tracks fail.
	Dynamic() (*MoqBroadcastDynamic, error)
	// Deprecated: use `close()`. A broadcast end carries no cause.
	Finish() error
	// Publish one audio codec as a new track.
	//
	// The track is [`MoqAudioInit::track`], or else named after the format (`0.opus`), so the
	// catalog is how a subscriber finds it. [`MoqAudioInit::data`] is required: audio resolves its
	// rendition entirely from those bytes.
	PublishAudio(init MoqAudioInit) (*MoqMediaProducer, error)
	// Publish one audio codec onto a track requested through
	// [`MoqBroadcastDynamic::requested_track`], which the importer accepts.
	PublishAudioOnTrack(request *MoqTrackRequest, init MoqAudioInit) (*MoqMediaProducer, error)
	// Publish a CMAF track whose fragments are delivered byte for byte.
	//
	// `init` is the track's CMAF init segment (ftyp+moov with exactly one trak). It becomes the
	// catalog rendition's CMAF container init unchanged, and the rest of the rendition (codec,
	// description, coded size, sample rate, channels) is derived from it as `publish_container`
	// derives it for fMP4. The catalog entry lasts as long as the track.
	PublishCmaf(name string, init []byte) (*MoqCmafProducer, error)
	// Publish a container, which demuxes and publishes its own tracks.
	//
	// Unlike the codec entry points there is no label or hint: a container describes each track it
	// publishes from its own metadata, so a rendition field would have no single track to land on.
	PublishContainer(init MoqContainerInit) (*MoqContainerProducer, error)
	// Publish a container fed by a raw byte stream, which recovers its own framing.
	PublishContainerStream(format MoqContainerFormat) (*MoqContainerStreamProducer, error)
	// Create a track for arbitrary byte payloads, no codec or container.
	//
	// Same pattern as moq-boy's `status` and `command` tracks: raw UTF-8/JSON
	// bytes written directly to moq-lite groups with no media framing. `info` sets
	// track properties (priority, max age, timescale); omit for defaults.
	PublishTrack(name string, info *MoqTrackInfo) (*MoqTrackProducer, error)
	// Publish one video codec as a new track.
	//
	// Named as in [`publish_audio`](Self::publish_audio). [`MoqVideoInit::data`] may be empty for a
	// format that resolves in band; a hint carrying the codec publishes the catalog before the
	// first keyframe.
	PublishVideo(init MoqVideoInit) (*MoqMediaProducer, error)
	// Publish one video codec onto a requested track. See
	// [`publish_audio_on_track`](Self::publish_audio_on_track).
	PublishVideoOnTrack(request *MoqTrackRequest, init MoqVideoInit) (*MoqMediaProducer, error)
	// Publish one video codec fed by a raw byte stream, inferring frame boundaries.
	//
	// Only the self-delimiting formats work here (`Avc3`, `Hev1`, `Av01`); the rest need length
	// prefixes or an out-of-band config record. There is no audio counterpart for the same reason.
	PublishVideoStream(init MoqVideoInit) (*MoqMediaStreamProducer, error)
	// Remove a top-level application catalog section by name.
	//
	// Republishes the catalog if the section existed; a no-op otherwise.
	RemoveCatalogSection(name string) error
	// Set (or replace) a top-level application catalog section by name.
	//
	// `json` is any JSON document (object, array, string, ...) serialized as a UTF-8 string.
	// Errors with [`MoqError::Json`] if `json` doesn't parse, or with the reserved-section
	// error if `name` is a HANG root (`video`, `audio`, `text`, `archive`, `clock`, `json`,
	// `binary`, or retired `timeline`) or an MSF root (`version`, `generatedAt`, `isComplete`,
	// `tracks`, or `initDataList`). The section is republished on the catalog track immediately.
	SetCatalogSection(name string, json string) error
	// Replace the catalog properties shared by every video rendition.
	//
	// Rotation is clockwise and normalized to the nearest quarter turn. An absent field is removed from the next catalog update.
	SetVideoProperties(properties MoqVideoProperties) error
	// Retract this broadcast's exact-path advertisement, if any.
	//
	// Local consumers and peers alike stop discovering and requesting it;
	// tracks already in flight carry on. Announcing again brings it back. A no-op
	// on a standalone broadcast.
	Unannounce() error
	// Open a video track on this broadcast, encoding the raw frames written to
	// it.
	//
	// The encoder opens here, so an unsupported codec, resolution, or backend
	// fails now rather than on the first frame. [`MoqVideoEncoderOutput::track`]
	// chooses the track name; `None` derives one from the codec. The catalog
	// rendition is published immediately so a subscriber can discover the track
	// before a frame is written to it.
	//
	// Pass `bandwidth` to reserve this track's configured bitrate against the
	// session's allocator and follow the grant with the same policy the Rust
	// capture encoder uses. [`set_bitrate`](MoqVideoProducer::set_bitrate) is
	// the manual ceiling: it retunes the encoder and moves the reservation.
	EncodeVideo(input MoqVideoEncoderInput, output MoqVideoEncoderOutput, bandwidth **MoqBandwidth) (*MoqVideoProducer, error)
}
type MoqBroadcastProducer struct {
	ffiObject FfiObject
}

// Create a standalone broadcast, not attached to any origin.
//
// Use it to serve a dynamic broadcast request ([`MoqBroadcastRequest::accept`](crate::origin::MoqBroadcastRequest::accept))
// or for local pub/sub via [`consume`](Self::consume). To publish at a path, use
// [`MoqOriginProducer::create_broadcast`](crate::origin::MoqOriginProducer::create_broadcast) instead.
func NewMoqBroadcastProducer() (*MoqBroadcastProducer, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_constructor_moqbroadcastproducer_new(_uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqBroadcastProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqBroadcastProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Open an audio track on this broadcast. The catalog rendition is
// registered immediately so subscribers can find the track even
// before the first frame is written.
//
// Pass `bandwidth` to reserve this track's bitrate against the session's
// allocator. Following the grant waits on the Rust audio producer; this
// call only claims the share so a co-resident video encoder sizes itself
// against what is left.
func (_self *MoqBroadcastProducer) EncodeAudio(name string, input MoqAudioEncoderInput, output MoqAudioEncoderOutput, bandwidth **MoqBandwidth) (*MoqAudioProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_encode_audio(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterMoqAudioEncoderInputINSTANCE.Lower(input), FfiConverterMoqAudioEncoderOutputINSTANCE.Lower(output), FfiConverterOptionalMoqBandwidthINSTANCE.Lower(bandwidth), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqAudioProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqAudioProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Publish a binary snapshot track (lossy latest-value) by name, advertised in the catalog.
//
// Errors if the catalog already carries an entry under `name`.
func (_self *MoqBroadcastProducer) PublishBinarySnapshot(name string, config MoqBinaryConfig) (*MoqBinarySnapshotProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_binary_snapshot(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterMoqBinaryConfigINSTANCE.Lower(config), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqBinarySnapshotProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqBinarySnapshotProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Publish a binary stream track (lossless append-log) by name, advertised in the catalog.
//
// Errors if the catalog already carries an entry under `name`.
func (_self *MoqBroadcastProducer) PublishBinaryStream(name string, config MoqBinaryConfig) (*MoqBinaryStreamProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_binary_stream(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterMoqBinaryConfigINSTANCE.Lower(config), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqBinaryStreamProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqBinaryStreamProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Publish a JSON snapshot track (lossy latest-value) by name, advertised in the catalog.
//
// The broadcast's catalog carries `json.tracks.<name>` (`mode: snapshot`, and
// `compression: deflate` when set) for as long as the track lives; finishing or dropping the
// producer retires it. Errors if the catalog already carries an entry under `name`.
func (_self *MoqBroadcastProducer) PublishJsonSnapshot(name string, config MoqJsonSnapshotConfig) (*MoqJsonSnapshotProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_json_snapshot(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterMoqJsonSnapshotConfigINSTANCE.Lower(config), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqJsonSnapshotProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqJsonSnapshotProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Publish a JSON stream track (lossless append-log) by name, advertised in the catalog.
//
// The broadcast's catalog carries `json.tracks.<name>` (`mode: stream`) for as long as the
// track lives. Errors if the catalog already carries an entry under `name`.
func (_self *MoqBroadcastProducer) PublishJsonStream(name string, config MoqJsonStreamConfig) (*MoqJsonStreamProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_json_stream(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterMoqJsonStreamConfigINSTANCE.Lower(config), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqJsonStreamProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqJsonStreamProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Advertise this broadcast's exact path as a route.
//
// Until announced, the broadcast is invisible and unroutable for local
// consumers and peers alike. Announcing again re-prices the route in place.
// Errors with `Closed` on a standalone broadcast (no origin to announce on).
func (_self *MoqBroadcastProducer) Announce(route MoqRoute) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_announce(
			_pointer, FfiConverterMoqRouteINSTANCE.Lower(route), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// End the broadcast for good: retract it, serve no new tracks, and finalize the catalog.
//
// Tracks already subscribed carry on to their own end. Every later call on this
// producer fails with `Closed`; closing again is a no-op.
func (_self *MoqBroadcastProducer) Close() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_close(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Create a consumer that reads from this broadcast's tracks.
func (_self *MoqBroadcastProducer) Consume() (*MoqBroadcastConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_consume(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqBroadcastConsumer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqBroadcastConsumerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a dynamic producer that yields tracks requested by subscribers.
//
// Hold the returned object for as long as missing track requests should be
// accepted. Dropping it makes future subscriptions to unknown tracks fail.
func (_self *MoqBroadcastProducer) Dynamic() (*MoqBroadcastDynamic, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_dynamic(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqBroadcastDynamic
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqBroadcastDynamicINSTANCE.Lift(_uniffiRV), nil
	}
}

// Deprecated: use `close()`. A broadcast end carries no cause.
func (_self *MoqBroadcastProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Publish one audio codec as a new track.
//
// The track is [`MoqAudioInit::track`], or else named after the format (`0.opus`), so the
// catalog is how a subscriber finds it. [`MoqAudioInit::data`] is required: audio resolves its
// rendition entirely from those bytes.
func (_self *MoqBroadcastProducer) PublishAudio(init MoqAudioInit) (*MoqMediaProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_audio(
			_pointer, FfiConverterMoqAudioInitINSTANCE.Lower(init), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqMediaProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqMediaProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Publish one audio codec onto a track requested through
// [`MoqBroadcastDynamic::requested_track`], which the importer accepts.
func (_self *MoqBroadcastProducer) PublishAudioOnTrack(request *MoqTrackRequest, init MoqAudioInit) (*MoqMediaProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_audio_on_track(
			_pointer, FfiConverterMoqTrackRequestINSTANCE.Lower(request), FfiConverterMoqAudioInitINSTANCE.Lower(init), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqMediaProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqMediaProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Publish a CMAF track whose fragments are delivered byte for byte.
//
// `init` is the track's CMAF init segment (ftyp+moov with exactly one trak). It becomes the
// catalog rendition's CMAF container init unchanged, and the rest of the rendition (codec,
// description, coded size, sample rate, channels) is derived from it as `publish_container`
// derives it for fMP4. The catalog entry lasts as long as the track.
func (_self *MoqBroadcastProducer) PublishCmaf(name string, init []byte) (*MoqCmafProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_cmaf(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterBytesINSTANCE.Lower(init), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqCmafProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqCmafProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Publish a container, which demuxes and publishes its own tracks.
//
// Unlike the codec entry points there is no label or hint: a container describes each track it
// publishes from its own metadata, so a rendition field would have no single track to land on.
func (_self *MoqBroadcastProducer) PublishContainer(init MoqContainerInit) (*MoqContainerProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_container(
			_pointer, FfiConverterMoqContainerInitINSTANCE.Lower(init), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqContainerProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqContainerProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Publish a container fed by a raw byte stream, which recovers its own framing.
func (_self *MoqBroadcastProducer) PublishContainerStream(format MoqContainerFormat) (*MoqContainerStreamProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_container_stream(
			_pointer, FfiConverterMoqContainerFormatINSTANCE.Lower(format), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqContainerStreamProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqContainerStreamProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a track for arbitrary byte payloads, no codec or container.
//
// Same pattern as moq-boy's `status` and `command` tracks: raw UTF-8/JSON
// bytes written directly to moq-lite groups with no media framing. `info` sets
// track properties (priority, max age, timescale); omit for defaults.
func (_self *MoqBroadcastProducer) PublishTrack(name string, info *MoqTrackInfo) (*MoqTrackProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_track(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterOptionalMoqTrackInfoINSTANCE.Lower(info), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqTrackProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Publish one video codec as a new track.
//
// Named as in [`publish_audio`](Self::publish_audio). [`MoqVideoInit::data`] may be empty for a
// format that resolves in band; a hint carrying the codec publishes the catalog before the
// first keyframe.
func (_self *MoqBroadcastProducer) PublishVideo(init MoqVideoInit) (*MoqMediaProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_video(
			_pointer, FfiConverterMoqVideoInitINSTANCE.Lower(init), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqMediaProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqMediaProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Publish one video codec onto a requested track. See
// [`publish_audio_on_track`](Self::publish_audio_on_track).
func (_self *MoqBroadcastProducer) PublishVideoOnTrack(request *MoqTrackRequest, init MoqVideoInit) (*MoqMediaProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_video_on_track(
			_pointer, FfiConverterMoqTrackRequestINSTANCE.Lower(request), FfiConverterMoqVideoInitINSTANCE.Lower(init), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqMediaProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqMediaProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Publish one video codec fed by a raw byte stream, inferring frame boundaries.
//
// Only the self-delimiting formats work here (`Avc3`, `Hev1`, `Av01`); the rest need length
// prefixes or an out-of-band config record. There is no audio counterpart for the same reason.
func (_self *MoqBroadcastProducer) PublishVideoStream(init MoqVideoInit) (*MoqMediaStreamProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_video_stream(
			_pointer, FfiConverterMoqVideoInitINSTANCE.Lower(init), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqMediaStreamProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqMediaStreamProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Remove a top-level application catalog section by name.
//
// Republishes the catalog if the section existed; a no-op otherwise.
func (_self *MoqBroadcastProducer) RemoveCatalogSection(name string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_remove_catalog_section(
			_pointer, FfiConverterStringINSTANCE.Lower(name), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Set (or replace) a top-level application catalog section by name.
//
// `json` is any JSON document (object, array, string, ...) serialized as a UTF-8 string.
// Errors with [`MoqError::Json`] if `json` doesn't parse, or with the reserved-section
// error if `name` is a HANG root (`video`, `audio`, `text`, `archive`, `clock`, `json`,
// `binary`, or retired `timeline`) or an MSF root (`version`, `generatedAt`, `isComplete`,
// `tracks`, or `initDataList`). The section is republished on the catalog track immediately.
func (_self *MoqBroadcastProducer) SetCatalogSection(name string, json string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_set_catalog_section(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterStringINSTANCE.Lower(json), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Replace the catalog properties shared by every video rendition.
//
// Rotation is clockwise and normalized to the nearest quarter turn. An absent field is removed from the next catalog update.
func (_self *MoqBroadcastProducer) SetVideoProperties(properties MoqVideoProperties) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_set_video_properties(
			_pointer, FfiConverterMoqVideoPropertiesINSTANCE.Lower(properties), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Retract this broadcast's exact-path advertisement, if any.
//
// Local consumers and peers alike stop discovering and requesting it;
// tracks already in flight carry on. Announcing again brings it back. A no-op
// on a standalone broadcast.
func (_self *MoqBroadcastProducer) Unannounce() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_unannounce(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Open a video track on this broadcast, encoding the raw frames written to
// it.
//
// The encoder opens here, so an unsupported codec, resolution, or backend
// fails now rather than on the first frame. [`MoqVideoEncoderOutput::track`]
// chooses the track name; `None` derives one from the codec. The catalog
// rendition is published immediately so a subscriber can discover the track
// before a frame is written to it.
//
// Pass `bandwidth` to reserve this track's configured bitrate against the
// session's allocator and follow the grant with the same policy the Rust
// capture encoder uses. [`set_bitrate`](MoqVideoProducer::set_bitrate) is
// the manual ceiling: it retunes the encoder and moves the reservation.
func (_self *MoqBroadcastProducer) EncodeVideo(input MoqVideoEncoderInput, output MoqVideoEncoderOutput, bandwidth **MoqBandwidth) (*MoqVideoProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_encode_video(
			_pointer, FfiConverterMoqVideoEncoderInputINSTANCE.Lower(input), FfiConverterMoqVideoEncoderOutputINSTANCE.Lower(output), FfiConverterOptionalMoqBandwidthINSTANCE.Lower(bandwidth), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqVideoProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqVideoProducerINSTANCE.Lift(_uniffiRV), nil
	}
}
func (object *MoqBroadcastProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqBroadcastProducer struct{}

var FfiConverterMoqBroadcastProducerINSTANCE = FfiConverterMoqBroadcastProducer{}

func (c FfiConverterMoqBroadcastProducer) Lift(handle C.uint64_t) *MoqBroadcastProducer {
	result := &MoqBroadcastProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqbroadcastproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqbroadcastproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqBroadcastProducer).Destroy)
	return result
}

func (c FfiConverterMoqBroadcastProducer) Read(reader io.Reader) *MoqBroadcastProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqBroadcastProducer) Lower(value *MoqBroadcastProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqBroadcastProducer) Write(writer io.Writer, value *MoqBroadcastProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqBroadcastProducer(handle uint64) *MoqBroadcastProducer {
	return FfiConverterMoqBroadcastProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqBroadcastProducer(value *MoqBroadcastProducer) uint64 {
	return uint64(FfiConverterMoqBroadcastProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqBroadcastProducer struct{}

func (_ FfiDestroyerMoqBroadcastProducer) Destroy(value *MoqBroadcastProducer) {
	value.Destroy()
}

// A pending dynamic broadcast request that must be accepted or rejected.
type MoqBroadcastRequestInterface interface {
	// Accept the request with an unannounced broadcast.
	Accept(broadcast *MoqBroadcastProducer) error
	// The requested broadcast path.
	Path() (string, error)
	// Reject the request with an application error code.
	Reject(errorCode uint16) error
}

// A pending dynamic broadcast request that must be accepted or rejected.
type MoqBroadcastRequest struct {
	ffiObject FfiObject
}

// Accept the request with an unannounced broadcast.
func (_self *MoqBroadcastRequest) Accept(broadcast *MoqBroadcastProducer) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastRequest")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastrequest_accept(
			_pointer, FfiConverterMoqBroadcastProducerINSTANCE.Lower(broadcast), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// The requested broadcast path.
func (_self *MoqBroadcastRequest) Path() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastRequest")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqbroadcastrequest_path(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Reject the request with an application error code.
func (_self *MoqBroadcastRequest) Reject(errorCode uint16) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastRequest")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastrequest_reject(
			_pointer, FfiConverterUint16INSTANCE.Lower(errorCode), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqBroadcastRequest) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqBroadcastRequest struct{}

var FfiConverterMoqBroadcastRequestINSTANCE = FfiConverterMoqBroadcastRequest{}

func (c FfiConverterMoqBroadcastRequest) Lift(handle C.uint64_t) *MoqBroadcastRequest {
	result := &MoqBroadcastRequest{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqbroadcastrequest(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqbroadcastrequest(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqBroadcastRequest).Destroy)
	return result
}

func (c FfiConverterMoqBroadcastRequest) Read(reader io.Reader) *MoqBroadcastRequest {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqBroadcastRequest) Lower(value *MoqBroadcastRequest) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqBroadcastRequest")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqBroadcastRequest) Write(writer io.Writer, value *MoqBroadcastRequest) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqBroadcastRequest(handle uint64) *MoqBroadcastRequest {
	return FfiConverterMoqBroadcastRequestINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqBroadcastRequest(value *MoqBroadcastRequest) uint64 {
	return uint64(FfiConverterMoqBroadcastRequestINSTANCE.Lower(value))
}

type FfiDestroyerMoqBroadcastRequest struct{}

func (_ FfiDestroyerMoqBroadcastRequest) Destroy(value *MoqBroadcastRequest) {
	value.Destroy()
}

type MoqCatalogConsumerInterface interface {
	// Cancel all current and future `next()` calls.
	//
	// Terminal: the subscription is released here, not when the handle is.
	Cancel()
	// Get the next catalog update. Returns `None` when the track ends or is closed.
	Next(
		ctx context.Context) (*MoqCatalog, error)
}
type MoqCatalogConsumer struct {
	ffiObject FfiObject
}

// Cancel all current and future `next()` calls.
//
// Terminal: the subscription is released here, not when the handle is.
func (_self *MoqCatalogConsumer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqCatalogConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqcatalogconsumer_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Get the next catalog update. Returns `None` when the track ends or is closed.
func (_self *MoqCatalogConsumer) Next(
	ctx context.Context) (*MoqCatalog, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqCatalogConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MoqCatalog {
			return FfiConverterOptionalMoqCatalogINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqcatalogconsumer_next(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_rust_buffer(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	return res, err
}
func (object *MoqCatalogConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqCatalogConsumer struct{}

var FfiConverterMoqCatalogConsumerINSTANCE = FfiConverterMoqCatalogConsumer{}

func (c FfiConverterMoqCatalogConsumer) Lift(handle C.uint64_t) *MoqCatalogConsumer {
	result := &MoqCatalogConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqcatalogconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqcatalogconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqCatalogConsumer).Destroy)
	return result
}

func (c FfiConverterMoqCatalogConsumer) Read(reader io.Reader) *MoqCatalogConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqCatalogConsumer) Lower(value *MoqCatalogConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqCatalogConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqCatalogConsumer) Write(writer io.Writer, value *MoqCatalogConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqCatalogConsumer(handle uint64) *MoqCatalogConsumer {
	return FfiConverterMoqCatalogConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqCatalogConsumer(value *MoqCatalogConsumer) uint64 {
	return uint64(FfiConverterMoqCatalogConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqCatalogConsumer struct{}

func (_ FfiDestroyerMoqCatalogConsumer) Destroy(value *MoqCatalogConsumer) {
	value.Destroy()
}

// Builds a [`MoqSession`]: configure it, then [`connect`](Self::connect).
//
// The configuration differs by target, because the transport does. Native builds expose
// the QUIC socket and TLS trust store; the browser owns both, so a wasm build exposes
// only the certificate hashes WebTransport accepts.
//
// Setters write the configuration [`connect`](Self::connect) will snapshot. They fail
// with [`MoqError::Busy`] while a connect is in flight and [`MoqError::Cancelled`]
// after [`cancel`](Self::cancel). A finished connect does not freeze the handle: later
// setters apply to the next dial until cancel.
type MoqClientInterface interface {
	// Cancel all current and future `connect()` calls.
	//
	// Terminal: the client's configuration and wired origins are released here, not when the
	// handle is, so this client can't dial again.
	Cancel()
	// Connect to a MoQ server and wait for the session to be established.
	//
	// The returned session automatically reconnects with backoff when the transport
	// drops (unless disabled via [`set_reconnect`](Self::set_reconnect)), and broadcasts
	// consumed through it ride out the gap. Watch [`MoqSession::status`] for the
	// connect/disconnect transitions, [`MoqSession::epoch`] for the reconnect count,
	// and [`MoqSession::closed`] for the connection giving up for good.
	//
	// Both origin sides are always accessible via [`MoqSession::publish`] and
	// [`MoqSession::consume`], without the caller constructing a [`MoqOriginProducer`]
	// themselves. With neither [`set_publish`](Self::set_publish) nor
	// [`set_consume`](Self::set_consume) wired, the two sides share one origin, so a broadcast
	// announced on this session is also discoverable through it. Wiring either side opts out of
	// that and gives the other side its own fresh origin.
	//
	// Can be cancelled by calling `cancel()`, including while the initial dial is retrying.
	Connect(
		ctx context.Context, url string) (*MoqSession, error)
	// Configure retry pacing for the automatic reconnect (see [`MoqBackoff`]).
	SetBackoff(backoff MoqBackoff) error
	// Set the local UDP socket bind address. Defaults to `[::]:0`.
	//
	// Returns an error if the address cannot be parsed, if a connect is in flight,
	// or after [`cancel`](Self::cancel).
	SetBind(addr string) error
	// Set the origin to consume remote broadcasts from the remote.
	SetConsume(origin **MoqOriginProducer) error
	// Set the origin to publish local broadcasts to the remote.
	SetPublish(origin **MoqOriginProducer) error
	// Cap the concurrent QUIC streams the peer may open toward this connection.
	// Defaults to 1024.
	//
	// MoQ opens a stream per group, and for a subscriber those arrive from the relay,
	// so a client subscribing to many tracks wants this raised. A publisher's own
	// streams are bounded by the peer's advertised limit, not this one. Ignored by
	// the WebSocket fallback.
	SetQuicMaxStreams(maxStreams uint64) error
	// Enable or disable automatic reconnecting. Enabled by default.
	//
	// When enabled, the session returned by [`connect`](Self::connect) redials with
	// backoff whenever the transport drops, and broadcasts consumed through it survive
	// the gap. Disable for a one-shot dial: the transport's close then ends the session
	// (surfaced via [`MoqSession::closed`]).
	SetReconnect(enabled bool) error
	// Present this PEM certificate chain when the relay requires mTLS.
	//
	// Only certificates are read from the file; any private keys are ignored. Must be
	// paired with `set_tls_key`, otherwise `connect` fails with an incomplete-auth error.
	// Pass `None` to clear a previously set path.
	SetTlsCert(path *string) error
	// Pin the peer to a certificate with one of these SHA-256 fingerprints, encoded as hex.
	//
	// This is the native equivalent of the browser's WebTransport `serverCertificateHashes`
	// and accepts the same values a server reports (see `MoqServer.cert_fingerprints`). Use it
	// to trust a self-signed certificate without disabling verification. An empty list clears
	// any pinned fingerprints.
	SetTlsFingerprints(fingerprints []string) error
	// Present this PEM private key when the relay requires mTLS.
	//
	// Only the private key is read from the file; any certificates are ignored. Must be
	// paired with `set_tls_cert`, otherwise `connect` fails with an incomplete-auth error.
	// Pass `None` to clear a previously set path.
	SetTlsKey(path *string) error
	// Trust these PEM root certificate file(s) instead of the system roots.
	//
	// Pass the paths to PEM-encoded CA certificates. An empty list restores the
	// default behavior of using the platform's native root store.
	SetTlsRoots(paths []string) error
	// Configure whether to also trust the platform's native root certificates.
	//
	// By default, system roots are trusted only when no custom roots are configured.
	// Set this to `true` to trust system roots in addition to roots from
	// `set_tls_roots`, or `false` to trust only custom roots.
	SetTlsSystemRoots(systemRoots bool) error
	// Enable or disable TLS certificate verification.
	SetTlsVerify(verify bool) error
	// Set the head start, in microseconds, QUIC gets before the WebSocket fallback joins
	// the race. Defaults to 200ms.
	//
	// Zero races both at once. A server where WebSocket already won skips the head start.
	SetWebsocketDelay(delayUs uint64) error
	// Enable or disable the WebSocket fallback. Enabled by default.
	//
	// The fallback races a WebSocket dial against QUIC for `http(s)` URLs, for networks
	// that block UDP. Disable it for a relay that only serves QUIC, so a failed QUIC dial
	// reports its own error instead of the fallback's.
	SetWebsocketEnabled(enabled bool) error
}

// Builds a [`MoqSession`]: configure it, then [`connect`](Self::connect).
//
// The configuration differs by target, because the transport does. Native builds expose
// the QUIC socket and TLS trust store; the browser owns both, so a wasm build exposes
// only the certificate hashes WebTransport accepts.
//
// Setters write the configuration [`connect`](Self::connect) will snapshot. They fail
// with [`MoqError::Busy`] while a connect is in flight and [`MoqError::Cancelled`]
// after [`cancel`](Self::cancel). A finished connect does not freeze the handle: later
// setters apply to the next dial until cancel.
type MoqClient struct {
	ffiObject FfiObject
}

// Create a new MoQ client with default configuration.
func NewMoqClient() *MoqClient {
	return FfiConverterMoqClientINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_constructor_moqclient_new(_uniffiStatus)
	}))
}

// Cancel all current and future `connect()` calls.
//
// Terminal: the client's configuration and wired origins are released here, not when the
// handle is, so this client can't dial again.
func (_self *MoqClient) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Connect to a MoQ server and wait for the session to be established.
//
// The returned session automatically reconnects with backoff when the transport
// drops (unless disabled via [`set_reconnect`](Self::set_reconnect)), and broadcasts
// consumed through it ride out the gap. Watch [`MoqSession::status`] for the
// connect/disconnect transitions, [`MoqSession::epoch`] for the reconnect count,
// and [`MoqSession::closed`] for the connection giving up for good.
//
// Both origin sides are always accessible via [`MoqSession::publish`] and
// [`MoqSession::consume`], without the caller constructing a [`MoqOriginProducer`]
// themselves. With neither [`set_publish`](Self::set_publish) nor
// [`set_consume`](Self::set_consume) wired, the two sides share one origin, so a broadcast
// announced on this session is also discoverable through it. Wiring either side opts out of
// that and gives the other side its own fresh origin.
//
// Can be cancelled by calling `cancel()`, including while the initial dial is retrying.
func (_self *MoqClient) Connect(
	ctx context.Context, url string) (*MoqSession, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqSession {
			return FfiConverterMoqSessionINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqclient_connect(
				_pointer, FfiConverterStringINSTANCE.Lower(url))
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_u64(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	return res, err
}

// Configure retry pacing for the automatic reconnect (see [`MoqBackoff`]).
func (_self *MoqClient) SetBackoff(backoff MoqBackoff) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_backoff(
			_pointer, FfiConverterMoqBackoffINSTANCE.Lower(backoff), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Set the local UDP socket bind address. Defaults to `[::]:0`.
//
// Returns an error if the address cannot be parsed, if a connect is in flight,
// or after [`cancel`](Self::cancel).
func (_self *MoqClient) SetBind(addr string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_bind(
			_pointer, FfiConverterStringINSTANCE.Lower(addr), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Set the origin to consume remote broadcasts from the remote.
func (_self *MoqClient) SetConsume(origin **MoqOriginProducer) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_consume(
			_pointer, FfiConverterOptionalMoqOriginProducerINSTANCE.Lower(origin), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Set the origin to publish local broadcasts to the remote.
func (_self *MoqClient) SetPublish(origin **MoqOriginProducer) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_publish(
			_pointer, FfiConverterOptionalMoqOriginProducerINSTANCE.Lower(origin), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Cap the concurrent QUIC streams the peer may open toward this connection.
// Defaults to 1024.
//
// MoQ opens a stream per group, and for a subscriber those arrive from the relay,
// so a client subscribing to many tracks wants this raised. A publisher's own
// streams are bounded by the peer's advertised limit, not this one. Ignored by
// the WebSocket fallback.
func (_self *MoqClient) SetQuicMaxStreams(maxStreams uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_quic_max_streams(
			_pointer, FfiConverterUint64INSTANCE.Lower(maxStreams), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Enable or disable automatic reconnecting. Enabled by default.
//
// When enabled, the session returned by [`connect`](Self::connect) redials with
// backoff whenever the transport drops, and broadcasts consumed through it survive
// the gap. Disable for a one-shot dial: the transport's close then ends the session
// (surfaced via [`MoqSession::closed`]).
func (_self *MoqClient) SetReconnect(enabled bool) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_reconnect(
			_pointer, FfiConverterBoolINSTANCE.Lower(enabled), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Present this PEM certificate chain when the relay requires mTLS.
//
// Only certificates are read from the file; any private keys are ignored. Must be
// paired with `set_tls_key`, otherwise `connect` fails with an incomplete-auth error.
// Pass `None` to clear a previously set path.
func (_self *MoqClient) SetTlsCert(path *string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_tls_cert(
			_pointer, FfiConverterOptionalStringINSTANCE.Lower(path), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Pin the peer to a certificate with one of these SHA-256 fingerprints, encoded as hex.
//
// This is the native equivalent of the browser's WebTransport `serverCertificateHashes`
// and accepts the same values a server reports (see `MoqServer.cert_fingerprints`). Use it
// to trust a self-signed certificate without disabling verification. An empty list clears
// any pinned fingerprints.
func (_self *MoqClient) SetTlsFingerprints(fingerprints []string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_tls_fingerprints(
			_pointer, FfiConverterSequenceStringINSTANCE.Lower(fingerprints), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Present this PEM private key when the relay requires mTLS.
//
// Only the private key is read from the file; any certificates are ignored. Must be
// paired with `set_tls_cert`, otherwise `connect` fails with an incomplete-auth error.
// Pass `None` to clear a previously set path.
func (_self *MoqClient) SetTlsKey(path *string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_tls_key(
			_pointer, FfiConverterOptionalStringINSTANCE.Lower(path), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Trust these PEM root certificate file(s) instead of the system roots.
//
// Pass the paths to PEM-encoded CA certificates. An empty list restores the
// default behavior of using the platform's native root store.
func (_self *MoqClient) SetTlsRoots(paths []string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_tls_roots(
			_pointer, FfiConverterSequenceStringINSTANCE.Lower(paths), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Configure whether to also trust the platform's native root certificates.
//
// By default, system roots are trusted only when no custom roots are configured.
// Set this to `true` to trust system roots in addition to roots from
// `set_tls_roots`, or `false` to trust only custom roots.
func (_self *MoqClient) SetTlsSystemRoots(systemRoots bool) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_tls_system_roots(
			_pointer, FfiConverterBoolINSTANCE.Lower(systemRoots), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Enable or disable TLS certificate verification.
func (_self *MoqClient) SetTlsVerify(verify bool) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_tls_verify(
			_pointer, FfiConverterBoolINSTANCE.Lower(verify), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Set the head start, in microseconds, QUIC gets before the WebSocket fallback joins
// the race. Defaults to 200ms.
//
// Zero races both at once. A server where WebSocket already won skips the head start.
func (_self *MoqClient) SetWebsocketDelay(delayUs uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_websocket_delay(
			_pointer, FfiConverterUint64INSTANCE.Lower(delayUs), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Enable or disable the WebSocket fallback. Enabled by default.
//
// The fallback races a WebSocket dial against QUIC for `http(s)` URLs, for networks
// that block UDP. Disable it for a relay that only serves QUIC, so a failed QUIC dial
// reports its own error instead of the fallback's.
func (_self *MoqClient) SetWebsocketEnabled(enabled bool) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_websocket_enabled(
			_pointer, FfiConverterBoolINSTANCE.Lower(enabled), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqClient) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqClient struct{}

var FfiConverterMoqClientINSTANCE = FfiConverterMoqClient{}

func (c FfiConverterMoqClient) Lift(handle C.uint64_t) *MoqClient {
	result := &MoqClient{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqclient(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqclient(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqClient).Destroy)
	return result
}

func (c FfiConverterMoqClient) Read(reader io.Reader) *MoqClient {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqClient) Lower(value *MoqClient) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqClient")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqClient) Write(writer io.Writer, value *MoqClient) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqClient(handle uint64) *MoqClient {
	return FfiConverterMoqClientINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqClient(value *MoqClient) uint64 {
	return uint64(FfiConverterMoqClientINSTANCE.Lower(value))
}

type FfiDestroyerMoqClient struct{}

func (_ FfiDestroyerMoqClient) Destroy(value *MoqClient) {
	value.Destroy()
}

// One group of a CMAF track, where each fragment is one frame.
type MoqCmafGroupProducerInterface interface {
	// Abort this group with an application error code.
	Abort(errorCode uint16) error
	// Mark the group as complete. No more fragments can be written.
	//
	// The handle remains so a later [`abort`](Self::abort) can still run.
	Finish() error
	// The sequence number of this group within the track.
	Sequence() uint64
	// Write one CMAF fragment as one frame, unchanged.
	//
	// A fragment is any leading top-level boxes (e.g. `uuid`), then exactly one `moof`, holding a
	// single `traf` for this track, and one `mdat`. The frame timestamp is the first sample's
	// presentation time (`tfdt` plus the first `trun` composition offset) on the init's `mdhd`
	// timescale. A malformed fragment is refused with nothing written.
	WriteFragment(fragment []byte) error
}

// One group of a CMAF track, where each fragment is one frame.
type MoqCmafGroupProducer struct {
	ffiObject FfiObject
}

// Abort this group with an application error code.
func (_self *MoqCmafGroupProducer) Abort(errorCode uint16) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqCmafGroupProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqcmafgroupproducer_abort(
			_pointer, FfiConverterUint16INSTANCE.Lower(errorCode), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Mark the group as complete. No more fragments can be written.
//
// The handle remains so a later [`abort`](Self::abort) can still run.
func (_self *MoqCmafGroupProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqCmafGroupProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqcmafgroupproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// The sequence number of this group within the track.
func (_self *MoqCmafGroupProducer) Sequence() uint64 {
	_pointer := _self.ffiObject.incrementPointer("*MoqCmafGroupProducer")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterUint64INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqcmafgroupproducer_sequence(
			_pointer, _uniffiStatus)
	}))
}

// Write one CMAF fragment as one frame, unchanged.
//
// A fragment is any leading top-level boxes (e.g. `uuid`), then exactly one `moof`, holding a
// single `traf` for this track, and one `mdat`. The frame timestamp is the first sample's
// presentation time (`tfdt` plus the first `trun` composition offset) on the init's `mdhd`
// timescale. A malformed fragment is refused with nothing written.
func (_self *MoqCmafGroupProducer) WriteFragment(fragment []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqCmafGroupProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqcmafgroupproducer_write_fragment(
			_pointer, FfiConverterBytesINSTANCE.Lower(fragment), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqCmafGroupProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqCmafGroupProducer struct{}

var FfiConverterMoqCmafGroupProducerINSTANCE = FfiConverterMoqCmafGroupProducer{}

func (c FfiConverterMoqCmafGroupProducer) Lift(handle C.uint64_t) *MoqCmafGroupProducer {
	result := &MoqCmafGroupProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqcmafgroupproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqcmafgroupproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqCmafGroupProducer).Destroy)
	return result
}

func (c FfiConverterMoqCmafGroupProducer) Read(reader io.Reader) *MoqCmafGroupProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqCmafGroupProducer) Lower(value *MoqCmafGroupProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqCmafGroupProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqCmafGroupProducer) Write(writer io.Writer, value *MoqCmafGroupProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqCmafGroupProducer(handle uint64) *MoqCmafGroupProducer {
	return FfiConverterMoqCmafGroupProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqCmafGroupProducer(value *MoqCmafGroupProducer) uint64 {
	return uint64(FfiConverterMoqCmafGroupProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqCmafGroupProducer struct{}

func (_ FfiDestroyerMoqCmafGroupProducer) Destroy(value *MoqCmafGroupProducer) {
	value.Destroy()
}

// A CMAF track whose fragments are delivered byte for byte.
//
// Made by [`MoqBroadcastProducer::publish_cmaf`]. The caller draws the groups. The track is not
// enrolled in the broadcast's timeline, and its rendition carries no measured bitrate or jitter.
type MoqCmafProducerInterface interface {
	// Abort the track with an application error code and retire its catalog entry.
	Abort(errorCode uint16) error
	// Append a new group after the latest one.
	AppendGroup() (*MoqCmafGroupProducer, error)
	// Create the group numbered `sequence`.
	CreateGroup(sequence uint64) (*MoqCmafGroupProducer, error)
	// End the track at the live edge and retire its catalog entry.
	Finish() error
}

// A CMAF track whose fragments are delivered byte for byte.
//
// Made by [`MoqBroadcastProducer::publish_cmaf`]. The caller draws the groups. The track is not
// enrolled in the broadcast's timeline, and its rendition carries no measured bitrate or jitter.
type MoqCmafProducer struct {
	ffiObject FfiObject
}

// Abort the track with an application error code and retire its catalog entry.
func (_self *MoqCmafProducer) Abort(errorCode uint16) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqCmafProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqcmafproducer_abort(
			_pointer, FfiConverterUint16INSTANCE.Lower(errorCode), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Append a new group after the latest one.
func (_self *MoqCmafProducer) AppendGroup() (*MoqCmafGroupProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqCmafProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqcmafproducer_append_group(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqCmafGroupProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqCmafGroupProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create the group numbered `sequence`.
func (_self *MoqCmafProducer) CreateGroup(sequence uint64) (*MoqCmafGroupProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqCmafProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqcmafproducer_create_group(
			_pointer, FfiConverterUint64INSTANCE.Lower(sequence), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqCmafGroupProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqCmafGroupProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// End the track at the live edge and retire its catalog entry.
func (_self *MoqCmafProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqCmafProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqcmafproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqCmafProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqCmafProducer struct{}

var FfiConverterMoqCmafProducerINSTANCE = FfiConverterMoqCmafProducer{}

func (c FfiConverterMoqCmafProducer) Lift(handle C.uint64_t) *MoqCmafProducer {
	result := &MoqCmafProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqcmafproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqcmafproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqCmafProducer).Destroy)
	return result
}

func (c FfiConverterMoqCmafProducer) Read(reader io.Reader) *MoqCmafProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqCmafProducer) Lower(value *MoqCmafProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqCmafProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqCmafProducer) Write(writer io.Writer, value *MoqCmafProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqCmafProducer(handle uint64) *MoqCmafProducer {
	return FfiConverterMoqCmafProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqCmafProducer(value *MoqCmafProducer) uint64 {
	return uint64(FfiConverterMoqCmafProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqCmafProducer struct{}

func (_ FfiDestroyerMoqCmafProducer) Destroy(value *MoqCmafProducer) {
	value.Destroy()
}

type MoqContainerProducerInterface interface {
	// Declare that the next chunk starts a new segment, rolling a group on every track this
	// publishes.
	//
	// For a caller that knows its source's segmentation out of band. An fMP4 source carrying
	// `styp` atoms declares its own, so this is only needed when it doesn't, and formats with no
	// segment concept (MKV, TS, FLV) ignore it.
	Cut() error
	// Finish every track this container publishes.
	Finish() error
	// Start a new segment and number its groups `sequence`.
	Seek(sequence uint64) error
	// Write a whole chunk of the container.
	//
	// No timestamp: a container carries its tracks' timing itself, and the importer reads it out
	// rather than taking the caller's word for it.
	Write(payload []byte) error
}
type MoqContainerProducer struct {
	ffiObject FfiObject
}

// Declare that the next chunk starts a new segment, rolling a group on every track this
// publishes.
//
// For a caller that knows its source's segmentation out of band. An fMP4 source carrying
// `styp` atoms declares its own, so this is only needed when it doesn't, and formats with no
// segment concept (MKV, TS, FLV) ignore it.
func (_self *MoqContainerProducer) Cut() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqContainerProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqcontainerproducer_cut(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Finish every track this container publishes.
func (_self *MoqContainerProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqContainerProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqcontainerproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Start a new segment and number its groups `sequence`.
func (_self *MoqContainerProducer) Seek(sequence uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqContainerProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqcontainerproducer_seek(
			_pointer, FfiConverterUint64INSTANCE.Lower(sequence), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Write a whole chunk of the container.
//
// No timestamp: a container carries its tracks' timing itself, and the importer reads it out
// rather than taking the caller's word for it.
func (_self *MoqContainerProducer) Write(payload []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqContainerProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqcontainerproducer_write(
			_pointer, FfiConverterBytesINSTANCE.Lower(payload), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqContainerProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqContainerProducer struct{}

var FfiConverterMoqContainerProducerINSTANCE = FfiConverterMoqContainerProducer{}

func (c FfiConverterMoqContainerProducer) Lift(handle C.uint64_t) *MoqContainerProducer {
	result := &MoqContainerProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqcontainerproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqcontainerproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqContainerProducer).Destroy)
	return result
}

func (c FfiConverterMoqContainerProducer) Read(reader io.Reader) *MoqContainerProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqContainerProducer) Lower(value *MoqContainerProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqContainerProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqContainerProducer) Write(writer io.Writer, value *MoqContainerProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqContainerProducer(handle uint64) *MoqContainerProducer {
	return FfiConverterMoqContainerProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqContainerProducer(value *MoqContainerProducer) uint64 {
	return uint64(FfiConverterMoqContainerProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqContainerProducer struct{}

func (_ FfiDestroyerMoqContainerProducer) Destroy(value *MoqContainerProducer) {
	value.Destroy()
}

type MoqContainerStreamProducerInterface interface {
	// Finish every track this container publishes.
	Finish() error
	// Push raw container bytes. The importer recovers its own framing, so callers can write
	// arbitrary chunks.
	Write(payload []byte) error
}
type MoqContainerStreamProducer struct {
	ffiObject FfiObject
}

// Finish every track this container publishes.
func (_self *MoqContainerStreamProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqContainerStreamProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqcontainerstreamproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Push raw container bytes. The importer recovers its own framing, so callers can write
// arbitrary chunks.
func (_self *MoqContainerStreamProducer) Write(payload []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqContainerStreamProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqcontainerstreamproducer_write(
			_pointer, FfiConverterBytesINSTANCE.Lower(payload), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqContainerStreamProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqContainerStreamProducer struct{}

var FfiConverterMoqContainerStreamProducerINSTANCE = FfiConverterMoqContainerStreamProducer{}

func (c FfiConverterMoqContainerStreamProducer) Lift(handle C.uint64_t) *MoqContainerStreamProducer {
	result := &MoqContainerStreamProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqcontainerstreamproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqcontainerstreamproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqContainerStreamProducer).Destroy)
	return result
}

func (c FfiConverterMoqContainerStreamProducer) Read(reader io.Reader) *MoqContainerStreamProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqContainerStreamProducer) Lower(value *MoqContainerStreamProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqContainerStreamProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqContainerStreamProducer) Write(writer io.Writer, value *MoqContainerStreamProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqContainerStreamProducer(handle uint64) *MoqContainerStreamProducer {
	return FfiConverterMoqContainerStreamProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqContainerStreamProducer(value *MoqContainerStreamProducer) uint64 {
	return uint64(FfiConverterMoqContainerStreamProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqContainerStreamProducer struct{}

func (_ FfiDestroyerMoqContainerStreamProducer) Destroy(value *MoqContainerStreamProducer) {
	value.Destroy()
}

type MoqGroupConsumerInterface interface {
	// Cancel all current and future `read_frame()` calls.
	//
	// Terminal: the group and whatever it still buffers are released here, not when the handle is.
	Cancel()
	// Read the next frame in this group, including its timestamp.
	//
	// Returns `None` when the group ends.
	ReadFrame(
		ctx context.Context) (*MoqFrame, error)
	// The sequence number of this group within the track.
	Sequence() uint64
}
type MoqGroupConsumer struct {
	ffiObject FfiObject
}

// Cancel all current and future `read_frame()` calls.
//
// Terminal: the group and whatever it still buffers are released here, not when the handle is.
func (_self *MoqGroupConsumer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqgroupconsumer_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Read the next frame in this group, including its timestamp.
//
// Returns `None` when the group ends.
func (_self *MoqGroupConsumer) ReadFrame(
	ctx context.Context) (*MoqFrame, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MoqFrame {
			return FfiConverterOptionalMoqFrameINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqgroupconsumer_read_frame(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_rust_buffer(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	return res, err
}

// The sequence number of this group within the track.
func (_self *MoqGroupConsumer) Sequence() uint64 {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupConsumer")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterUint64INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqgroupconsumer_sequence(
			_pointer, _uniffiStatus)
	}))
}
func (object *MoqGroupConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqGroupConsumer struct{}

var FfiConverterMoqGroupConsumerINSTANCE = FfiConverterMoqGroupConsumer{}

func (c FfiConverterMoqGroupConsumer) Lift(handle C.uint64_t) *MoqGroupConsumer {
	result := &MoqGroupConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqgroupconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqgroupconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqGroupConsumer).Destroy)
	return result
}

func (c FfiConverterMoqGroupConsumer) Read(reader io.Reader) *MoqGroupConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqGroupConsumer) Lower(value *MoqGroupConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqGroupConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqGroupConsumer) Write(writer io.Writer, value *MoqGroupConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqGroupConsumer(handle uint64) *MoqGroupConsumer {
	return FfiConverterMoqGroupConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqGroupConsumer(value *MoqGroupConsumer) uint64 {
	return uint64(FfiConverterMoqGroupConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqGroupConsumer struct{}

func (_ FfiDestroyerMoqGroupConsumer) Destroy(value *MoqGroupConsumer) {
	value.Destroy()
}

type MoqGroupProducerInterface interface {
	// Abort this group with an application error code.
	Abort(errorCode uint16) error
	// Create a consumer that reads frames from this group.
	Consume() (*MoqGroupConsumer, error)
	// Mark the group as complete. No more frames can be written.
	//
	// The handle remains so a later [`abort`](Self::abort) can still run.
	Finish() error
	// The sequence number of this group within the track.
	Sequence() uint64
	// Write `frame` into this group.
	//
	// Raw tracks default to a microsecond timescale. Custom timescales may round
	// the timestamp during conversion.
	WriteFrame(frame MoqFrame) error
}
type MoqGroupProducer struct {
	ffiObject FfiObject
}

// Abort this group with an application error code.
func (_self *MoqGroupProducer) Abort(errorCode uint16) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqgroupproducer_abort(
			_pointer, FfiConverterUint16INSTANCE.Lower(errorCode), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Create a consumer that reads frames from this group.
func (_self *MoqGroupProducer) Consume() (*MoqGroupConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqgroupproducer_consume(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqGroupConsumer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqGroupConsumerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Mark the group as complete. No more frames can be written.
//
// The handle remains so a later [`abort`](Self::abort) can still run.
func (_self *MoqGroupProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqgroupproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// The sequence number of this group within the track.
func (_self *MoqGroupProducer) Sequence() uint64 {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupProducer")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterUint64INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqgroupproducer_sequence(
			_pointer, _uniffiStatus)
	}))
}

// Write `frame` into this group.
//
// Raw tracks default to a microsecond timescale. Custom timescales may round
// the timestamp during conversion.
func (_self *MoqGroupProducer) WriteFrame(frame MoqFrame) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqgroupproducer_write_frame(
			_pointer, FfiConverterMoqFrameINSTANCE.Lower(frame), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqGroupProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqGroupProducer struct{}

var FfiConverterMoqGroupProducerINSTANCE = FfiConverterMoqGroupProducer{}

func (c FfiConverterMoqGroupProducer) Lift(handle C.uint64_t) *MoqGroupProducer {
	result := &MoqGroupProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqgroupproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqgroupproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqGroupProducer).Destroy)
	return result
}

func (c FfiConverterMoqGroupProducer) Read(reader io.Reader) *MoqGroupProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqGroupProducer) Lower(value *MoqGroupProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqGroupProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqGroupProducer) Write(writer io.Writer, value *MoqGroupProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqGroupProducer(handle uint64) *MoqGroupProducer {
	return FfiConverterMoqGroupProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqGroupProducer(value *MoqGroupProducer) uint64 {
	return uint64(FfiConverterMoqGroupProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqGroupProducer struct{}

func (_ FfiDestroyerMoqGroupProducer) Destroy(value *MoqGroupProducer) {
	value.Destroy()
}

// An uncached group requested by a fetch consumer.
type MoqGroupRequestInterface interface {
	// Reject the fetch with an application error code.
	Abort(errorCode uint16) error
	// Accept the request and return a producer for filling the fetched group.
	Accept() (*MoqGroupProducer, error)
	// The consumer's delivery priority for this fetch.
	Priority() uint8
	// The requested group sequence within the track.
	Sequence() uint64
}

// An uncached group requested by a fetch consumer.
type MoqGroupRequest struct {
	ffiObject FfiObject
}

// Reject the fetch with an application error code.
func (_self *MoqGroupRequest) Abort(errorCode uint16) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupRequest")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqgrouprequest_abort(
			_pointer, FfiConverterUint16INSTANCE.Lower(errorCode), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Accept the request and return a producer for filling the fetched group.
func (_self *MoqGroupRequest) Accept() (*MoqGroupProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupRequest")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqgrouprequest_accept(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqGroupProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqGroupProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// The consumer's delivery priority for this fetch.
func (_self *MoqGroupRequest) Priority() uint8 {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterUint8INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint8_t {
		return C.uniffi_moq_ffi_fn_method_moqgrouprequest_priority(
			_pointer, _uniffiStatus)
	}))
}

// The requested group sequence within the track.
func (_self *MoqGroupRequest) Sequence() uint64 {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterUint64INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqgrouprequest_sequence(
			_pointer, _uniffiStatus)
	}))
}
func (object *MoqGroupRequest) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqGroupRequest struct{}

var FfiConverterMoqGroupRequestINSTANCE = FfiConverterMoqGroupRequest{}

func (c FfiConverterMoqGroupRequest) Lift(handle C.uint64_t) *MoqGroupRequest {
	result := &MoqGroupRequest{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqgrouprequest(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqgrouprequest(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqGroupRequest).Destroy)
	return result
}

func (c FfiConverterMoqGroupRequest) Read(reader io.Reader) *MoqGroupRequest {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqGroupRequest) Lower(value *MoqGroupRequest) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqGroupRequest")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqGroupRequest) Write(writer io.Writer, value *MoqGroupRequest) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqGroupRequest(handle uint64) *MoqGroupRequest {
	return FfiConverterMoqGroupRequestINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqGroupRequest(value *MoqGroupRequest) uint64 {
	return uint64(FfiConverterMoqGroupRequestINSTANCE.Lower(value))
}

type FfiDestroyerMoqGroupRequest struct{}

func (_ FfiDestroyerMoqGroupRequest) Destroy(value *MoqGroupRequest) {
	value.Destroy()
}

// Consumes a JSON snapshot track, yielding the latest reconstructed value.
type MoqJsonSnapshotConsumerInterface interface {
	// Cancel all current and future `next()` calls.
	//
	// Terminal: the subscription is released here, not when the handle is.
	Cancel()
	// Get the next value as a JSON string. Returns `None` once the track ends.
	//
	// A consumer that has fallen behind collapses the backlog and yields only the latest value.
	Next(
		ctx context.Context) (*string, error)
}

// Consumes a JSON snapshot track, yielding the latest reconstructed value.
type MoqJsonSnapshotConsumer struct {
	ffiObject FfiObject
}

// Cancel all current and future `next()` calls.
//
// Terminal: the subscription is released here, not when the handle is.
func (_self *MoqJsonSnapshotConsumer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonSnapshotConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqjsonsnapshotconsumer_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Get the next value as a JSON string. Returns `None` once the track ends.
//
// A consumer that has fallen behind collapses the backlog and yields only the latest value.
func (_self *MoqJsonSnapshotConsumer) Next(
	ctx context.Context) (*string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonSnapshotConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *string {
			return FfiConverterOptionalStringINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqjsonsnapshotconsumer_next(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_rust_buffer(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	return res, err
}
func (object *MoqJsonSnapshotConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqJsonSnapshotConsumer struct{}

var FfiConverterMoqJsonSnapshotConsumerINSTANCE = FfiConverterMoqJsonSnapshotConsumer{}

func (c FfiConverterMoqJsonSnapshotConsumer) Lift(handle C.uint64_t) *MoqJsonSnapshotConsumer {
	result := &MoqJsonSnapshotConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqjsonsnapshotconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqjsonsnapshotconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqJsonSnapshotConsumer).Destroy)
	return result
}

func (c FfiConverterMoqJsonSnapshotConsumer) Read(reader io.Reader) *MoqJsonSnapshotConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqJsonSnapshotConsumer) Lower(value *MoqJsonSnapshotConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqJsonSnapshotConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqJsonSnapshotConsumer) Write(writer io.Writer, value *MoqJsonSnapshotConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqJsonSnapshotConsumer(handle uint64) *MoqJsonSnapshotConsumer {
	return FfiConverterMoqJsonSnapshotConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqJsonSnapshotConsumer(value *MoqJsonSnapshotConsumer) uint64 {
	return uint64(FfiConverterMoqJsonSnapshotConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqJsonSnapshotConsumer struct{}

func (_ FfiDestroyerMoqJsonSnapshotConsumer) Destroy(value *MoqJsonSnapshotConsumer) {
	value.Destroy()
}

// Publishes a JSON value that consumers see as a single latest state.
type MoqJsonSnapshotProducerInterface interface {
	// A watch-only handle to whether this track has subscribers.
	Demand() (*MoqTrackDemand, error)
	// Finish the track, closing any open group.
	Finish() error
	// Publish a new value, encoded as a snapshot or delta automatically. `value` is a JSON
	// document. A no-op if unchanged from the previous update.
	Update(value string) error
}

// Publishes a JSON value that consumers see as a single latest state.
type MoqJsonSnapshotProducer struct {
	ffiObject FfiObject
}

// A watch-only handle to whether this track has subscribers.
func (_self *MoqJsonSnapshotProducer) Demand() (*MoqTrackDemand, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonSnapshotProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqjsonsnapshotproducer_demand(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqTrackDemand
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackDemandINSTANCE.Lift(_uniffiRV), nil
	}
}

// Finish the track, closing any open group.
func (_self *MoqJsonSnapshotProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonSnapshotProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqjsonsnapshotproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Publish a new value, encoded as a snapshot or delta automatically. `value` is a JSON
// document. A no-op if unchanged from the previous update.
func (_self *MoqJsonSnapshotProducer) Update(value string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonSnapshotProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqjsonsnapshotproducer_update(
			_pointer, FfiConverterStringINSTANCE.Lower(value), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqJsonSnapshotProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqJsonSnapshotProducer struct{}

var FfiConverterMoqJsonSnapshotProducerINSTANCE = FfiConverterMoqJsonSnapshotProducer{}

func (c FfiConverterMoqJsonSnapshotProducer) Lift(handle C.uint64_t) *MoqJsonSnapshotProducer {
	result := &MoqJsonSnapshotProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqjsonsnapshotproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqjsonsnapshotproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqJsonSnapshotProducer).Destroy)
	return result
}

func (c FfiConverterMoqJsonSnapshotProducer) Read(reader io.Reader) *MoqJsonSnapshotProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqJsonSnapshotProducer) Lower(value *MoqJsonSnapshotProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqJsonSnapshotProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqJsonSnapshotProducer) Write(writer io.Writer, value *MoqJsonSnapshotProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqJsonSnapshotProducer(handle uint64) *MoqJsonSnapshotProducer {
	return FfiConverterMoqJsonSnapshotProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqJsonSnapshotProducer(value *MoqJsonSnapshotProducer) uint64 {
	return uint64(FfiConverterMoqJsonSnapshotProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqJsonSnapshotProducer struct{}

func (_ FfiDestroyerMoqJsonSnapshotProducer) Destroy(value *MoqJsonSnapshotProducer) {
	value.Destroy()
}

// Consumes an ordered log of JSON records, yielding every record in order.
type MoqJsonStreamConsumerInterface interface {
	// Cancel all current and future `next()` calls.
	//
	// Terminal: the subscription is released here, not when the handle is.
	Cancel()
	// Get the next record as a JSON string. Returns `None` once the track ends.
	Next(
		ctx context.Context) (*string, error)
}

// Consumes an ordered log of JSON records, yielding every record in order.
type MoqJsonStreamConsumer struct {
	ffiObject FfiObject
}

// Cancel all current and future `next()` calls.
//
// Terminal: the subscription is released here, not when the handle is.
func (_self *MoqJsonStreamConsumer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonStreamConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqjsonstreamconsumer_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Get the next record as a JSON string. Returns `None` once the track ends.
func (_self *MoqJsonStreamConsumer) Next(
	ctx context.Context) (*string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonStreamConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *string {
			return FfiConverterOptionalStringINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqjsonstreamconsumer_next(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_rust_buffer(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	return res, err
}
func (object *MoqJsonStreamConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqJsonStreamConsumer struct{}

var FfiConverterMoqJsonStreamConsumerINSTANCE = FfiConverterMoqJsonStreamConsumer{}

func (c FfiConverterMoqJsonStreamConsumer) Lift(handle C.uint64_t) *MoqJsonStreamConsumer {
	result := &MoqJsonStreamConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqjsonstreamconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqjsonstreamconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqJsonStreamConsumer).Destroy)
	return result
}

func (c FfiConverterMoqJsonStreamConsumer) Read(reader io.Reader) *MoqJsonStreamConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqJsonStreamConsumer) Lower(value *MoqJsonStreamConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqJsonStreamConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqJsonStreamConsumer) Write(writer io.Writer, value *MoqJsonStreamConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqJsonStreamConsumer(handle uint64) *MoqJsonStreamConsumer {
	return FfiConverterMoqJsonStreamConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqJsonStreamConsumer(value *MoqJsonStreamConsumer) uint64 {
	return uint64(FfiConverterMoqJsonStreamConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqJsonStreamConsumer struct{}

func (_ FfiDestroyerMoqJsonStreamConsumer) Destroy(value *MoqJsonStreamConsumer) {
	value.Destroy()
}

// Publishes an ordered log of JSON records, one record per append.
type MoqJsonStreamProducerInterface interface {
	// Append one record to the log. `value` is a JSON document.
	Append(value string) error
	// A watch-only handle to whether this track has subscribers.
	Demand() (*MoqTrackDemand, error)
	// Finish the track, closing the group.
	Finish() error
}

// Publishes an ordered log of JSON records, one record per append.
type MoqJsonStreamProducer struct {
	ffiObject FfiObject
}

// Append one record to the log. `value` is a JSON document.
func (_self *MoqJsonStreamProducer) Append(value string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonStreamProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqjsonstreamproducer_append(
			_pointer, FfiConverterStringINSTANCE.Lower(value), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// A watch-only handle to whether this track has subscribers.
func (_self *MoqJsonStreamProducer) Demand() (*MoqTrackDemand, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonStreamProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqjsonstreamproducer_demand(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqTrackDemand
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackDemandINSTANCE.Lift(_uniffiRV), nil
	}
}

// Finish the track, closing the group.
func (_self *MoqJsonStreamProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonStreamProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqjsonstreamproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqJsonStreamProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqJsonStreamProducer struct{}

var FfiConverterMoqJsonStreamProducerINSTANCE = FfiConverterMoqJsonStreamProducer{}

func (c FfiConverterMoqJsonStreamProducer) Lift(handle C.uint64_t) *MoqJsonStreamProducer {
	result := &MoqJsonStreamProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqjsonstreamproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqjsonstreamproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqJsonStreamProducer).Destroy)
	return result
}

func (c FfiConverterMoqJsonStreamProducer) Read(reader io.Reader) *MoqJsonStreamProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqJsonStreamProducer) Lower(value *MoqJsonStreamProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqJsonStreamProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqJsonStreamProducer) Write(writer io.Writer, value *MoqJsonStreamProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqJsonStreamProducer(handle uint64) *MoqJsonStreamProducer {
	return FfiConverterMoqJsonStreamProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqJsonStreamProducer(value *MoqJsonStreamProducer) uint64 {
	return uint64(FfiConverterMoqJsonStreamProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqJsonStreamProducer struct{}

func (_ FfiDestroyerMoqJsonStreamProducer) Destroy(value *MoqJsonStreamProducer) {
	value.Destroy()
}

type MoqMediaConsumerInterface interface {
	// Cancel all current and future `next()` calls.
	//
	// Terminal: the subscription is released here, not when the handle is.
	Cancel()
	// Get the next frame. Returns `None` when the track ends or is closed.
	Next(
		ctx context.Context) (*MoqMediaFrame, error)
}
type MoqMediaConsumer struct {
	ffiObject FfiObject
}

// Cancel all current and future `next()` calls.
//
// Terminal: the subscription is released here, not when the handle is.
func (_self *MoqMediaConsumer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqmediaconsumer_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Get the next frame. Returns `None` when the track ends or is closed.
func (_self *MoqMediaConsumer) Next(
	ctx context.Context) (*MoqMediaFrame, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MoqMediaFrame {
			return FfiConverterOptionalMoqMediaFrameINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqmediaconsumer_next(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_rust_buffer(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	return res, err
}
func (object *MoqMediaConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqMediaConsumer struct{}

var FfiConverterMoqMediaConsumerINSTANCE = FfiConverterMoqMediaConsumer{}

func (c FfiConverterMoqMediaConsumer) Lift(handle C.uint64_t) *MoqMediaConsumer {
	result := &MoqMediaConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqmediaconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqmediaconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqMediaConsumer).Destroy)
	return result
}

func (c FfiConverterMoqMediaConsumer) Read(reader io.Reader) *MoqMediaConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqMediaConsumer) Lower(value *MoqMediaConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqMediaConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqMediaConsumer) Write(writer io.Writer, value *MoqMediaConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqMediaConsumer(handle uint64) *MoqMediaConsumer {
	return FfiConverterMoqMediaConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqMediaConsumer(value *MoqMediaConsumer) uint64 {
	return uint64(FfiConverterMoqMediaConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqMediaConsumer struct{}

func (_ FfiDestroyerMoqMediaConsumer) Destroy(value *MoqMediaConsumer) {
	value.Destroy()
}

// A finite, container-decoded media group returned by
// [`MoqBroadcastConsumer::fetch_media_group`].
type MoqMediaGroupConsumerInterface interface {
	// Cancel all current and future `next()` calls.
	//
	// Terminal: the subscription is released here, not when the handle is.
	Cancel()
	// Read the next decoded media frame, or `None` when the group ends.
	Next(
		ctx context.Context) (*MoqMediaFrame, error)
	// The sequence number of this group within the track.
	Sequence() uint64
}

// A finite, container-decoded media group returned by
// [`MoqBroadcastConsumer::fetch_media_group`].
type MoqMediaGroupConsumer struct {
	ffiObject FfiObject
}

// Cancel all current and future `next()` calls.
//
// Terminal: the subscription is released here, not when the handle is.
func (_self *MoqMediaGroupConsumer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaGroupConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqmediagroupconsumer_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Read the next decoded media frame, or `None` when the group ends.
func (_self *MoqMediaGroupConsumer) Next(
	ctx context.Context) (*MoqMediaFrame, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaGroupConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MoqMediaFrame {
			return FfiConverterOptionalMoqMediaFrameINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqmediagroupconsumer_next(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_rust_buffer(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	return res, err
}

// The sequence number of this group within the track.
func (_self *MoqMediaGroupConsumer) Sequence() uint64 {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaGroupConsumer")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterUint64INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqmediagroupconsumer_sequence(
			_pointer, _uniffiStatus)
	}))
}
func (object *MoqMediaGroupConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqMediaGroupConsumer struct{}

var FfiConverterMoqMediaGroupConsumerINSTANCE = FfiConverterMoqMediaGroupConsumer{}

func (c FfiConverterMoqMediaGroupConsumer) Lift(handle C.uint64_t) *MoqMediaGroupConsumer {
	result := &MoqMediaGroupConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqmediagroupconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqmediagroupconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqMediaGroupConsumer).Destroy)
	return result
}

func (c FfiConverterMoqMediaGroupConsumer) Read(reader io.Reader) *MoqMediaGroupConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqMediaGroupConsumer) Lower(value *MoqMediaGroupConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqMediaGroupConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqMediaGroupConsumer) Write(writer io.Writer, value *MoqMediaGroupConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqMediaGroupConsumer(handle uint64) *MoqMediaGroupConsumer {
	return FfiConverterMoqMediaGroupConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqMediaGroupConsumer(value *MoqMediaGroupConsumer) uint64 {
	return uint64(FfiConverterMoqMediaGroupConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqMediaGroupConsumer struct{}

func (_ FfiDestroyerMoqMediaGroupConsumer) Destroy(value *MoqMediaGroupConsumer) {
	value.Destroy()
}

type MoqMediaProducerInterface interface {
	// Draw a group boundary here.
	//
	// Audio has no boundary of its own (every packet is independently decodable), so this is the
	// only thing that gives it groups: call it after every frame for one group (one QUIC stream)
	// the relay forwards without waiting, or at a segment cadence to align with video for
	// HLS/DASH. Video groups at its own keyframes and needs this only to override that.
	Cut() error
	// A watch-only handle to whether this track has subscribers.
	Demand() (*MoqTrackDemand, error)
	// Mark a timeline break and restart handoff measurement without lowering advertised jitter.
	//
	// Publishes a discontinuity marker; resumed frames must continue the broadcast media clock,
	// and video must resume on a keyframe.
	Discontinuity() error
	// Finish this track and finalize encoding.
	Finish() error
	// Record a locally encoded frame's handoff for catalog jitter measurement.
	//
	// `timestamp_us` is on the broadcast media clock. Call this after `write_frame` only for
	// encoder output; imported files, pipes, and network media stay clock-free.
	Flush(timestampUs uint64) error
	// The name of the track this publishes.
	Name() (string, error)
	// Draw a group boundary and number the next group `sequence`.
	//
	// [`cut`](Self::cut) with an explicit sequence, for a publisher whose group numbers have to
	// be deterministic: two encoders aligning per GOP so a consumer can fail over between them.
	Seek(sequence uint64) error
	// Wait until this track has no active consumers.
	//
	// Prefer [`demand`](Self::demand), a handle that can wait without borrowing this producer.
	Unused(
		ctx context.Context) error
	// Wait until this track has at least one active consumer.
	//
	// Prefer [`demand`](Self::demand), a handle that can wait without borrowing this producer.
	Used(
		ctx context.Context) error
	// Write `frame` to this track.
	//
	// The importer derives keyframe status from the bitstream, so a [`MoqFrame`] carries only the
	// payload and its timestamp.
	WriteFrame(frame MoqFrame) error
}
type MoqMediaProducer struct {
	ffiObject FfiObject
}

// Draw a group boundary here.
//
// Audio has no boundary of its own (every packet is independently decodable), so this is the
// only thing that gives it groups: call it after every frame for one group (one QUIC stream)
// the relay forwards without waiting, or at a segment cadence to align with video for
// HLS/DASH. Video groups at its own keyframes and needs this only to override that.
func (_self *MoqMediaProducer) Cut() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqmediaproducer_cut(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// A watch-only handle to whether this track has subscribers.
func (_self *MoqMediaProducer) Demand() (*MoqTrackDemand, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqmediaproducer_demand(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqTrackDemand
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackDemandINSTANCE.Lift(_uniffiRV), nil
	}
}

// Mark a timeline break and restart handoff measurement without lowering advertised jitter.
//
// Publishes a discontinuity marker; resumed frames must continue the broadcast media clock,
// and video must resume on a keyframe.
func (_self *MoqMediaProducer) Discontinuity() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqmediaproducer_discontinuity(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Finish this track and finalize encoding.
func (_self *MoqMediaProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqmediaproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Record a locally encoded frame's handoff for catalog jitter measurement.
//
// `timestamp_us` is on the broadcast media clock. Call this after `write_frame` only for
// encoder output; imported files, pipes, and network media stay clock-free.
func (_self *MoqMediaProducer) Flush(timestampUs uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqmediaproducer_flush(
			_pointer, FfiConverterUint64INSTANCE.Lower(timestampUs), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// The name of the track this publishes.
func (_self *MoqMediaProducer) Name() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqmediaproducer_name(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Draw a group boundary and number the next group `sequence`.
//
// [`cut`](Self::cut) with an explicit sequence, for a publisher whose group numbers have to
// be deterministic: two encoders aligning per GOP so a consumer can fail over between them.
func (_self *MoqMediaProducer) Seek(sequence uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqmediaproducer_seek(
			_pointer, FfiConverterUint64INSTANCE.Lower(sequence), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Wait until this track has no active consumers.
//
// Prefer [`demand`](Self::demand), a handle that can wait without borrowing this producer.
func (_self *MoqMediaProducer) Unused(
	ctx context.Context) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaProducer")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqmediaproducer_unused(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_void(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	return err
}

// Wait until this track has at least one active consumer.
//
// Prefer [`demand`](Self::demand), a handle that can wait without borrowing this producer.
func (_self *MoqMediaProducer) Used(
	ctx context.Context) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaProducer")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqmediaproducer_used(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_void(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	return err
}

// Write `frame` to this track.
//
// The importer derives keyframe status from the bitstream, so a [`MoqFrame`] carries only the
// payload and its timestamp.
func (_self *MoqMediaProducer) WriteFrame(frame MoqFrame) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqmediaproducer_write_frame(
			_pointer, FfiConverterMoqFrameINSTANCE.Lower(frame), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqMediaProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqMediaProducer struct{}

var FfiConverterMoqMediaProducerINSTANCE = FfiConverterMoqMediaProducer{}

func (c FfiConverterMoqMediaProducer) Lift(handle C.uint64_t) *MoqMediaProducer {
	result := &MoqMediaProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqmediaproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqmediaproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqMediaProducer).Destroy)
	return result
}

func (c FfiConverterMoqMediaProducer) Read(reader io.Reader) *MoqMediaProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqMediaProducer) Lower(value *MoqMediaProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqMediaProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqMediaProducer) Write(writer io.Writer, value *MoqMediaProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqMediaProducer(handle uint64) *MoqMediaProducer {
	return FfiConverterMoqMediaProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqMediaProducer(value *MoqMediaProducer) uint64 {
	return uint64(FfiConverterMoqMediaProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqMediaProducer struct{}

func (_ FfiDestroyerMoqMediaProducer) Destroy(value *MoqMediaProducer) {
	value.Destroy()
}

type MoqMediaStreamProducerInterface interface {
	// Finalize the track.
	//
	// The importer emits each access unit when the *next* one's start code arrives, so a trailing
	// access unit with no following delimiter (e.g. the last frame at EOF) is not emitted. This
	// matches moq-cli's stdin path.
	Finish() error
	// Push raw stream bytes (e.g. Annex-B H.264 from an encoder). The importer frames whole access
	// units and keeps any partial trailing frame for the next call, so callers can write arbitrary
	// chunks.
	Write(payload []byte) error
}
type MoqMediaStreamProducer struct {
	ffiObject FfiObject
}

// Finalize the track.
//
// The importer emits each access unit when the *next* one's start code arrives, so a trailing
// access unit with no following delimiter (e.g. the last frame at EOF) is not emitted. This
// matches moq-cli's stdin path.
func (_self *MoqMediaStreamProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaStreamProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqmediastreamproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Push raw stream bytes (e.g. Annex-B H.264 from an encoder). The importer frames whole access
// units and keeps any partial trailing frame for the next call, so callers can write arbitrary
// chunks.
func (_self *MoqMediaStreamProducer) Write(payload []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaStreamProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqmediastreamproducer_write(
			_pointer, FfiConverterBytesINSTANCE.Lower(payload), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqMediaStreamProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqMediaStreamProducer struct{}

var FfiConverterMoqMediaStreamProducerINSTANCE = FfiConverterMoqMediaStreamProducer{}

func (c FfiConverterMoqMediaStreamProducer) Lift(handle C.uint64_t) *MoqMediaStreamProducer {
	result := &MoqMediaStreamProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqmediastreamproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqmediastreamproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqMediaStreamProducer).Destroy)
	return result
}

func (c FfiConverterMoqMediaStreamProducer) Read(reader io.Reader) *MoqMediaStreamProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqMediaStreamProducer) Lower(value *MoqMediaStreamProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqMediaStreamProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqMediaStreamProducer) Write(writer io.Writer, value *MoqMediaStreamProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqMediaStreamProducer(handle uint64) *MoqMediaStreamProducer {
	return FfiConverterMoqMediaStreamProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqMediaStreamProducer(value *MoqMediaStreamProducer) uint64 {
	return uint64(FfiConverterMoqMediaStreamProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqMediaStreamProducer struct{}

func (_ FfiDestroyerMoqMediaStreamProducer) Destroy(value *MoqMediaStreamProducer) {
	value.Destroy()
}

type MoqOriginConsumerInterface interface {
	// Subscribe to routes matching a pattern scope; updates stay relative to the origin.
	Announced(config MoqAnnounceConfig) (*MoqAnnounceConsumer, error)
	// Resolve the broadcast at `path`, waiting until something can serve it.
	//
	// This is how you resolve a path right after connecting: announcements arrive over the
	// session after it opens, so `request_broadcast` on its own races them. A
	// broadcast created on this origin resolves once it is announced, like a
	// remote one.
	AnnouncedBroadcast(path string) (*MoqAnnouncedBroadcast, error)
	// Request a broadcast by path, resolving as soon as it can be served.
	//
	// Resolves through the best announced route covering the path: an announced broadcast
	// on this origin, a route a session announced (served on demand by that session), or a
	// dynamic handler on the origin. The most specific prefix wins, then the cheapest. An
	// unannounced broadcast is unroutable. Unlike `announced_broadcast`, this answers for what is
	// reachable *now* and errors if nothing can serve the path. Drop the returned future to
	// cancel.
	//
	// Calling this straight after connecting therefore races the session's announcements
	// and can report a live broadcast as unroutable. Await `announced_broadcast` first.
	RequestBroadcast(
		ctx context.Context, path string) (*MoqBroadcastConsumer, error)
}
type MoqOriginConsumer struct {
	ffiObject FfiObject
}

// Subscribe to routes matching a pattern scope; updates stay relative to the origin.
func (_self *MoqOriginConsumer) Announced(config MoqAnnounceConfig) (*MoqAnnounceConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqOriginConsumer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqoriginconsumer_announced(
			_pointer, FfiConverterMoqAnnounceConfigINSTANCE.Lower(config), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqAnnounceConsumer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqAnnounceConsumerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Resolve the broadcast at `path`, waiting until something can serve it.
//
// This is how you resolve a path right after connecting: announcements arrive over the
// session after it opens, so `request_broadcast` on its own races them. A
// broadcast created on this origin resolves once it is announced, like a
// remote one.
func (_self *MoqOriginConsumer) AnnouncedBroadcast(path string) (*MoqAnnouncedBroadcast, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqOriginConsumer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqoriginconsumer_announced_broadcast(
			_pointer, FfiConverterStringINSTANCE.Lower(path), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqAnnouncedBroadcast
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqAnnouncedBroadcastINSTANCE.Lift(_uniffiRV), nil
	}
}

// Request a broadcast by path, resolving as soon as it can be served.
//
// Resolves through the best announced route covering the path: an announced broadcast
// on this origin, a route a session announced (served on demand by that session), or a
// dynamic handler on the origin. The most specific prefix wins, then the cheapest. An
// unannounced broadcast is unroutable. Unlike `announced_broadcast`, this answers for what is
// reachable *now* and errors if nothing can serve the path. Drop the returned future to
// cancel.
//
// Calling this straight after connecting therefore races the session's announcements
// and can report a live broadcast as unroutable. Await `announced_broadcast` first.
func (_self *MoqOriginConsumer) RequestBroadcast(
	ctx context.Context, path string) (*MoqBroadcastConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqOriginConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqBroadcastConsumer {
			return FfiConverterMoqBroadcastConsumerINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqoriginconsumer_request_broadcast(
				_pointer, FfiConverterStringINSTANCE.Lower(path))
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_u64(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	return res, err
}
func (object *MoqOriginConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqOriginConsumer struct{}

var FfiConverterMoqOriginConsumerINSTANCE = FfiConverterMoqOriginConsumer{}

func (c FfiConverterMoqOriginConsumer) Lift(handle C.uint64_t) *MoqOriginConsumer {
	result := &MoqOriginConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqoriginconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqoriginconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqOriginConsumer).Destroy)
	return result
}

func (c FfiConverterMoqOriginConsumer) Read(reader io.Reader) *MoqOriginConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqOriginConsumer) Lower(value *MoqOriginConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqOriginConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqOriginConsumer) Write(writer io.Writer, value *MoqOriginConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqOriginConsumer(handle uint64) *MoqOriginConsumer {
	return FfiConverterMoqOriginConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqOriginConsumer(value *MoqOriginConsumer) uint64 {
	return uint64(FfiConverterMoqOriginConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqOriginConsumer struct{}

func (_ FfiDestroyerMoqOriginConsumer) Destroy(value *MoqOriginConsumer) {
	value.Destroy()
}

// A served route: advertises a path prefix and yields the broadcast requests
// beneath it for the application to accept or reject.
//
// Keeps its origin running, like a published broadcast, after every
// `MoqOriginProducer` is gone.
type MoqOriginDynamicInterface interface {
	// Stop serving and retract the route. Terminal: this handler is released
	// here, not when the handle is, so pending requests are rejected before
	// this returns.
	Cancel()
	// Wait for the next broadcast requested through this handle's prefix.
	//
	// Returns a [`MoqBroadcastRequest`]: accept it with a broadcast producer or reject
	// it with an application error code. The requesting consumer stays pending until then.
	RequestedBroadcast(
		ctx context.Context) (*MoqBroadcastRequest, error)
	// Re-price the route in place: replace its hops and costs. The prefix cannot
	// change; call `dynamic` again instead.
	Update(route MoqRoute) error
}

// A served route: advertises a path prefix and yields the broadcast requests
// beneath it for the application to accept or reject.
//
// Keeps its origin running, like a published broadcast, after every
// `MoqOriginProducer` is gone.
type MoqOriginDynamic struct {
	ffiObject FfiObject
}

// Stop serving and retract the route. Terminal: this handler is released
// here, not when the handle is, so pending requests are rejected before
// this returns.
func (_self *MoqOriginDynamic) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqOriginDynamic")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqorigindynamic_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Wait for the next broadcast requested through this handle's prefix.
//
// Returns a [`MoqBroadcastRequest`]: accept it with a broadcast producer or reject
// it with an application error code. The requesting consumer stays pending until then.
func (_self *MoqOriginDynamic) RequestedBroadcast(
	ctx context.Context) (*MoqBroadcastRequest, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqOriginDynamic")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqBroadcastRequest {
			return FfiConverterMoqBroadcastRequestINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqorigindynamic_requested_broadcast(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_u64(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	return res, err
}

// Re-price the route in place: replace its hops and costs. The prefix cannot
// change; call `dynamic` again instead.
func (_self *MoqOriginDynamic) Update(route MoqRoute) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqOriginDynamic")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqorigindynamic_update(
			_pointer, FfiConverterMoqRouteINSTANCE.Lower(route), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqOriginDynamic) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqOriginDynamic struct{}

var FfiConverterMoqOriginDynamicINSTANCE = FfiConverterMoqOriginDynamic{}

func (c FfiConverterMoqOriginDynamic) Lift(handle C.uint64_t) *MoqOriginDynamic {
	result := &MoqOriginDynamic{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqorigindynamic(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqorigindynamic(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqOriginDynamic).Destroy)
	return result
}

func (c FfiConverterMoqOriginDynamic) Read(reader io.Reader) *MoqOriginDynamic {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqOriginDynamic) Lower(value *MoqOriginDynamic) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqOriginDynamic")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqOriginDynamic) Write(writer io.Writer, value *MoqOriginDynamic) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqOriginDynamic(handle uint64) *MoqOriginDynamic {
	return FfiConverterMoqOriginDynamicINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqOriginDynamic(value *MoqOriginDynamic) uint64 {
	return uint64(FfiConverterMoqOriginDynamicINSTANCE.Lower(value))
}

type FfiDestroyerMoqOriginDynamic struct{}

func (_ FfiDestroyerMoqOriginDynamic) Destroy(value *MoqOriginDynamic) {
	value.Destroy()
}

type MoqOriginProducerInterface interface {
	// Create a consumer for this origin.
	Consume() *MoqOriginConsumer
	// Create a broadcast at `path` on this origin, returning the producer that feeds it.
	//
	// The broadcast exists for nobody, on this origin or its peers, until
	// [`MoqBroadcastProducer::announce`]: until then announcement streams skip it
	// and requests for its path are unroutable. Announce after populating
	// tracks; an on-demand handler is [`Self::dynamic`]. Create, `dynamic()` if
	// tracks are served on demand, populate, then announce.
	//
	// [`MoqBroadcastProducer::finish`] unpublishes immediately. Dropping the producer
	// without finishing also unpublishes, but subscribers observe the end as a
	// failure rather than a deliberate one.
	CreateBroadcast(path string) (*MoqBroadcastProducer, error)
	// Advertise `prefix` and serve the requests beneath it.
	//
	// A route claims `prefix` and every path beneath it (the empty prefix
	// claims every path). A service that only serves some of them advertises
	// the covering prefix and rejects the rest as they are requested. Hold
	// the returned handle while the route should stay advertised and missing
	// broadcasts should be served. Create, attach this for tracks served on
	// demand, populate, then announce.
	Dynamic(prefix string, route MoqRoute) (*MoqOriginDynamic, error)
}
type MoqOriginProducer struct {
	ffiObject FfiObject
}

// Create a new origin for publishing and/or consuming broadcasts.
func NewMoqOriginProducer(config MoqOriginConfig) *MoqOriginProducer {
	return FfiConverterMoqOriginProducerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_constructor_moqoriginproducer_new(FfiConverterMoqOriginConfigINSTANCE.Lower(config), _uniffiStatus)
	}))
}

// Create a consumer for this origin.
func (_self *MoqOriginProducer) Consume() *MoqOriginConsumer {
	_pointer := _self.ffiObject.incrementPointer("*MoqOriginProducer")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMoqOriginConsumerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqoriginproducer_consume(
			_pointer, _uniffiStatus)
	}))
}

// Create a broadcast at `path` on this origin, returning the producer that feeds it.
//
// The broadcast exists for nobody, on this origin or its peers, until
// [`MoqBroadcastProducer::announce`]: until then announcement streams skip it
// and requests for its path are unroutable. Announce after populating
// tracks; an on-demand handler is [`Self::dynamic`]. Create, `dynamic()` if
// tracks are served on demand, populate, then announce.
//
// [`MoqBroadcastProducer::finish`] unpublishes immediately. Dropping the producer
// without finishing also unpublishes, but subscribers observe the end as a
// failure rather than a deliberate one.
func (_self *MoqOriginProducer) CreateBroadcast(path string) (*MoqBroadcastProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqOriginProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqoriginproducer_create_broadcast(
			_pointer, FfiConverterStringINSTANCE.Lower(path), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqBroadcastProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqBroadcastProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Advertise `prefix` and serve the requests beneath it.
//
// A route claims `prefix` and every path beneath it (the empty prefix
// claims every path). A service that only serves some of them advertises
// the covering prefix and rejects the rest as they are requested. Hold
// the returned handle while the route should stay advertised and missing
// broadcasts should be served. Create, attach this for tracks served on
// demand, populate, then announce.
func (_self *MoqOriginProducer) Dynamic(prefix string, route MoqRoute) (*MoqOriginDynamic, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqOriginProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqoriginproducer_dynamic(
			_pointer, FfiConverterStringINSTANCE.Lower(prefix), FfiConverterMoqRouteINSTANCE.Lower(route), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqOriginDynamic
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqOriginDynamicINSTANCE.Lift(_uniffiRV), nil
	}
}
func (object *MoqOriginProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqOriginProducer struct{}

var FfiConverterMoqOriginProducerINSTANCE = FfiConverterMoqOriginProducer{}

func (c FfiConverterMoqOriginProducer) Lift(handle C.uint64_t) *MoqOriginProducer {
	result := &MoqOriginProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqoriginproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqoriginproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqOriginProducer).Destroy)
	return result
}

func (c FfiConverterMoqOriginProducer) Read(reader io.Reader) *MoqOriginProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqOriginProducer) Lower(value *MoqOriginProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqOriginProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqOriginProducer) Write(writer io.Writer, value *MoqOriginProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqOriginProducer(handle uint64) *MoqOriginProducer {
	return FfiConverterMoqOriginProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqOriginProducer(value *MoqOriginProducer) uint64 {
	return uint64(FfiConverterMoqOriginProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqOriginProducer struct{}

func (_ FfiDestroyerMoqOriginProducer) Destroy(value *MoqOriginProducer) {
	value.Destroy()
}

// An incoming MoQ session that can be accepted or rejected.
//
// Origin overrides are captured at [`accept`](Self::accept). Setters fail with
// [`MoqError::Busy`] while accept/reject is in flight, [`MoqError::AlreadyResponded`]
// after a response, and [`MoqError::Cancelled`] after [`cancel`](Self::cancel).
type MoqRequestInterface interface {
	// Complete the MoQ handshake and return the established session.
	//
	// Returns `AlreadyResponded` if `accept()` or `reject()` has already been called.
	Accept(
		ctx context.Context) (*MoqSession, error)
	// Cancel any in-flight `accept()` or `reject()` call.
	//
	// Terminal: an unanswered request is dropped here rather than when the handle is, which
	// rejects the session.
	Cancel()
	// The query-free request path, or empty for the root/missing path.
	Path() string
	// The encoded request query without the leading `?`, if present.
	Query() *string
	// Reject the established MoQ session with an application error code.
	//
	// Codes 401 and 403 map to the protocol's unauthorized error; every other
	// code is sent as an application error.
	//
	// Returns `AlreadyResponded` if `accept()` or `reject()` has already been called.
	Reject(
		ctx context.Context, code uint16) error
	// Override the consume origin for this session. Falls back to the server's
	// configured consume origin if unset. Captured at [`accept`](Self::accept).
	SetConsume(origin **MoqOriginProducer) error
	// Override the publish origin for this session. Falls back to the server's
	// configured publish origin if unset. Captured at [`accept`](Self::accept).
	SetPublish(origin **MoqOriginProducer) error
	// The network transport carrying this session.
	Transport() MoqTransport
	// The URL provided by the client, if any.
	Url() *string
}

// An incoming MoQ session that can be accepted or rejected.
//
// Origin overrides are captured at [`accept`](Self::accept). Setters fail with
// [`MoqError::Busy`] while accept/reject is in flight, [`MoqError::AlreadyResponded`]
// after a response, and [`MoqError::Cancelled`] after [`cancel`](Self::cancel).
type MoqRequest struct {
	ffiObject FfiObject
}

// Complete the MoQ handshake and return the established session.
//
// Returns `AlreadyResponded` if `accept()` or `reject()` has already been called.
func (_self *MoqRequest) Accept(
	ctx context.Context) (*MoqSession, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqRequest")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqSession {
			return FfiConverterMoqSessionINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqrequest_accept(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_u64(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	return res, err
}

// Cancel any in-flight `accept()` or `reject()` call.
//
// Terminal: an unanswered request is dropped here rather than when the handle is, which
// rejects the session.
func (_self *MoqRequest) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqRequest")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqrequest_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// The query-free request path, or empty for the root/missing path.
func (_self *MoqRequest) Path() string {
	_pointer := _self.ffiObject.incrementPointer("*MoqRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqrequest_path(
				_pointer, _uniffiStatus),
		}
	}))
}

// The encoded request query without the leading `?`, if present.
func (_self *MoqRequest) Query() *string {
	_pointer := _self.ffiObject.incrementPointer("*MoqRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqrequest_query(
				_pointer, _uniffiStatus),
		}
	}))
}

// Reject the established MoQ session with an application error code.
//
// Codes 401 and 403 map to the protocol's unauthorized error; every other
// code is sent as an application error.
//
// Returns `AlreadyResponded` if `accept()` or `reject()` has already been called.
func (_self *MoqRequest) Reject(
	ctx context.Context, code uint16) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqRequest")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqrequest_reject(
				_pointer, FfiConverterUint16INSTANCE.Lower(code))
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_void(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	return err
}

// Override the consume origin for this session. Falls back to the server's
// configured consume origin if unset. Captured at [`accept`](Self::accept).
func (_self *MoqRequest) SetConsume(origin **MoqOriginProducer) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqRequest")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqrequest_set_consume(
			_pointer, FfiConverterOptionalMoqOriginProducerINSTANCE.Lower(origin), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Override the publish origin for this session. Falls back to the server's
// configured publish origin if unset. Captured at [`accept`](Self::accept).
func (_self *MoqRequest) SetPublish(origin **MoqOriginProducer) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqRequest")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqrequest_set_publish(
			_pointer, FfiConverterOptionalMoqOriginProducerINSTANCE.Lower(origin), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// The network transport carrying this session.
func (_self *MoqRequest) Transport() MoqTransport {
	_pointer := _self.ffiObject.incrementPointer("*MoqRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMoqTransportINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqrequest_transport(
				_pointer, _uniffiStatus),
		}
	}))
}

// The URL provided by the client, if any.
func (_self *MoqRequest) Url() *string {
	_pointer := _self.ffiObject.incrementPointer("*MoqRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqrequest_url(
				_pointer, _uniffiStatus),
		}
	}))
}
func (object *MoqRequest) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqRequest struct{}

var FfiConverterMoqRequestINSTANCE = FfiConverterMoqRequest{}

func (c FfiConverterMoqRequest) Lift(handle C.uint64_t) *MoqRequest {
	result := &MoqRequest{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqrequest(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqrequest(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqRequest).Destroy)
	return result
}

func (c FfiConverterMoqRequest) Read(reader io.Reader) *MoqRequest {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqRequest) Lower(value *MoqRequest) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqRequest")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqRequest) Write(writer io.Writer, value *MoqRequest) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqRequest(handle uint64) *MoqRequest {
	return FfiConverterMoqRequestINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqRequest(value *MoqRequest) uint64 {
	return uint64(FfiConverterMoqRequestINSTANCE.Lower(value))
}

type FfiDestroyerMoqRequest struct{}

func (_ FfiDestroyerMoqRequest) Destroy(value *MoqRequest) {
	value.Destroy()
}

// One track's standing claim on a [`MoqBandwidth`].
//
// [`grant`](Self::grant) is [`moq_net::bandwidth::Reservation::peek`]: `None`
// means no estimate or no demand, so hold the current rate, and `Some(0)` is a
// real zero grant.
type MoqReservationInterface interface {
	// This reservation's slice right now, in bits per second.
	//
	// `None` means no estimate or no demand: hold the current rate. `Some(0)`
	// is a real zero grant.
	Grant() *uint64
	// Change the ceiling, keeping the same claim.
	Update(maxBps uint64)
}

// One track's standing claim on a [`MoqBandwidth`].
//
// [`grant`](Self::grant) is [`moq_net::bandwidth::Reservation::peek`]: `None`
// means no estimate or no demand, so hold the current rate, and `Some(0)` is a
// real zero grant.
type MoqReservation struct {
	ffiObject FfiObject
}

// This reservation's slice right now, in bits per second.
//
// `None` means no estimate or no demand: hold the current rate. `Some(0)`
// is a real zero grant.
func (_self *MoqReservation) Grant() *uint64 {
	_pointer := _self.ffiObject.incrementPointer("*MoqReservation")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalUint64INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqreservation_grant(
				_pointer, _uniffiStatus),
		}
	}))
}

// Change the ceiling, keeping the same claim.
func (_self *MoqReservation) Update(maxBps uint64) {
	_pointer := _self.ffiObject.incrementPointer("*MoqReservation")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqreservation_update(
			_pointer, FfiConverterUint64INSTANCE.Lower(maxBps), _uniffiStatus)
		return false
	})
}
func (object *MoqReservation) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqReservation struct{}

var FfiConverterMoqReservationINSTANCE = FfiConverterMoqReservation{}

func (c FfiConverterMoqReservation) Lift(handle C.uint64_t) *MoqReservation {
	result := &MoqReservation{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqreservation(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqreservation(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqReservation).Destroy)
	return result
}

func (c FfiConverterMoqReservation) Read(reader io.Reader) *MoqReservation {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqReservation) Lower(value *MoqReservation) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqReservation")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqReservation) Write(writer io.Writer, value *MoqReservation) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqReservation(handle uint64) *MoqReservation {
	return FfiConverterMoqReservationINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqReservation(value *MoqReservation) uint64 {
	return uint64(FfiConverterMoqReservationINSTANCE.Lower(value))
}

type FfiDestroyerMoqReservation struct{}

func (_ FfiDestroyerMoqReservation) Destroy(value *MoqReservation) {
	value.Destroy()
}

// A MoQ server that accepts incoming QUIC/WebTransport sessions.
//
// Bind and TLS are captured at [`listen`](Self::listen); those setters fail
// afterwards. Origins are captured at each [`accept`](Self::accept). Every setter
// fails with [`MoqError::Busy`] while listen/accept is in flight and
// [`MoqError::Cancelled`] after [`cancel`](Self::cancel).
type MoqServerInterface interface {
	// Accept the next incoming session. Returns `None` when the server has closed.
	//
	// `listen()` must be called first. Dropping the returned future aborts this
	// call alone and leaves the server listening.
	Accept(
		ctx context.Context) (**MoqRequest, error)
	// Cancel any in-flight `listen()` or `accept()` call.
	//
	// Terminal, and synchronous: it returns once the listening socket is closed,
	// not when the handle is, so the address can be bound again immediately.
	// `cert_fingerprints()` returns `Cancelled` afterwards.
	Cancel()
	// SHA-256 fingerprints of the configured TLS certificates, hex-encoded.
	//
	// Useful for pinning a generated self-signed certificate in a browser via
	// WebTransport's `serverCertificateHashes`. Returns an error if called
	// before `listen()`.
	CertFingerprints() ([]string, error)
	// Bind the listening socket. Returns the bound local address as a string,
	// which is useful when binding to an ephemeral port (`:0`).
	Listen(
		ctx context.Context) (string, error)
	// Set the address to bind, e.g. `127.0.0.1:4443`, `[::]:443`, or `localhost:0`.
	//
	// Validated syntactically up-front. DNS hostnames are accepted and resolved
	// at `listen()` time. Captured at [`listen`](Self::listen); fails afterwards.
	SetBind(addr string) error
	// Set the origin to consume broadcasts from incoming sessions.
	//
	// Captured at each [`accept`](Self::accept).
	SetConsume(origin **MoqOriginProducer) error
	// Set the origin to publish broadcasts to incoming sessions.
	//
	// Captured at each [`accept`](Self::accept).
	SetPublish(origin **MoqOriginProducer) error
	// Load TLS certificate chains from PEM files on disk.
	//
	// Captured at [`listen`](Self::listen); fails afterwards.
	SetTlsCert(paths []string) error
	// Generate self-signed TLS certificates for the given hostnames.
	//
	// Clients must either pin the certificate fingerprint or disable verification.
	// Captured at [`listen`](Self::listen); fails afterwards.
	SetTlsGenerate(hostnames []string) error
	// Load TLS private keys from PEM files on disk.
	//
	// Captured at [`listen`](Self::listen); fails afterwards.
	SetTlsKey(paths []string) error
}

// A MoQ server that accepts incoming QUIC/WebTransport sessions.
//
// Bind and TLS are captured at [`listen`](Self::listen); those setters fail
// afterwards. Origins are captured at each [`accept`](Self::accept). Every setter
// fails with [`MoqError::Busy`] while listen/accept is in flight and
// [`MoqError::Cancelled`] after [`cancel`](Self::cancel).
type MoqServer struct {
	ffiObject FfiObject
}

// Create a new MoQ server with default configuration.
func NewMoqServer() *MoqServer {
	return FfiConverterMoqServerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_constructor_moqserver_new(_uniffiStatus)
	}))
}

// Accept the next incoming session. Returns `None` when the server has closed.
//
// `listen()` must be called first. Dropping the returned future aborts this
// call alone and leaves the server listening.
func (_self *MoqServer) Accept(
	ctx context.Context) (**MoqRequest, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) **MoqRequest {
			return FfiConverterOptionalMoqRequestINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqserver_accept(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_rust_buffer(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	return res, err
}

// Cancel any in-flight `listen()` or `accept()` call.
//
// Terminal, and synchronous: it returns once the listening socket is closed,
// not when the handle is, so the address can be bound again immediately.
// `cert_fingerprints()` returns `Cancelled` afterwards.
func (_self *MoqServer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqserver_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// SHA-256 fingerprints of the configured TLS certificates, hex-encoded.
//
// Useful for pinning a generated self-signed certificate in a browser via
// WebTransport's `serverCertificateHashes`. Returns an error if called
// before `listen()`.
func (_self *MoqServer) CertFingerprints() ([]string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqserver_cert_fingerprints(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue []string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterSequenceStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Bind the listening socket. Returns the bound local address as a string,
// which is useful when binding to an ephemeral port (`:0`).
func (_self *MoqServer) Listen(
	ctx context.Context) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) string {
			return FfiConverterStringINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqserver_listen(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_rust_buffer(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	return res, err
}

// Set the address to bind, e.g. `127.0.0.1:4443`, `[::]:443`, or `localhost:0`.
//
// Validated syntactically up-front. DNS hostnames are accepted and resolved
// at `listen()` time. Captured at [`listen`](Self::listen); fails afterwards.
func (_self *MoqServer) SetBind(addr string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqserver_set_bind(
			_pointer, FfiConverterStringINSTANCE.Lower(addr), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Set the origin to consume broadcasts from incoming sessions.
//
// Captured at each [`accept`](Self::accept).
func (_self *MoqServer) SetConsume(origin **MoqOriginProducer) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqserver_set_consume(
			_pointer, FfiConverterOptionalMoqOriginProducerINSTANCE.Lower(origin), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Set the origin to publish broadcasts to incoming sessions.
//
// Captured at each [`accept`](Self::accept).
func (_self *MoqServer) SetPublish(origin **MoqOriginProducer) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqserver_set_publish(
			_pointer, FfiConverterOptionalMoqOriginProducerINSTANCE.Lower(origin), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Load TLS certificate chains from PEM files on disk.
//
// Captured at [`listen`](Self::listen); fails afterwards.
func (_self *MoqServer) SetTlsCert(paths []string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqserver_set_tls_cert(
			_pointer, FfiConverterSequenceStringINSTANCE.Lower(paths), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Generate self-signed TLS certificates for the given hostnames.
//
// Clients must either pin the certificate fingerprint or disable verification.
// Captured at [`listen`](Self::listen); fails afterwards.
func (_self *MoqServer) SetTlsGenerate(hostnames []string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqserver_set_tls_generate(
			_pointer, FfiConverterSequenceStringINSTANCE.Lower(hostnames), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Load TLS private keys from PEM files on disk.
//
// Captured at [`listen`](Self::listen); fails afterwards.
func (_self *MoqServer) SetTlsKey(paths []string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqserver_set_tls_key(
			_pointer, FfiConverterSequenceStringINSTANCE.Lower(paths), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqServer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqServer struct{}

var FfiConverterMoqServerINSTANCE = FfiConverterMoqServer{}

func (c FfiConverterMoqServer) Lift(handle C.uint64_t) *MoqServer {
	result := &MoqServer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqserver(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqserver(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqServer).Destroy)
	return result
}

func (c FfiConverterMoqServer) Read(reader io.Reader) *MoqServer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqServer) Lower(value *MoqServer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqServer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqServer) Write(writer io.Writer, value *MoqServer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqServer(handle uint64) *MoqServer {
	return FfiConverterMoqServerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqServer(value *MoqServer) uint64 {
	return uint64(FfiConverterMoqServerINSTANCE.Lower(value))
}

type FfiDestroyerMoqServer struct{}

func (_ FfiDestroyerMoqServer) Destroy(value *MoqServer) {
	value.Destroy()
}

type MoqSessionInterface interface {
	// The session's bandwidth allocator, used to divide the connection's send
	// estimate among tracks sharing it.
	//
	// Every call returns a handle to the same registry, so reservations made
	// through one are visible to the others. A client handle survives
	// reconnects: the grant is `None` while disconnected and resumes on the
	// next connection. An accepted session with no congestion estimate mints
	// an unlimited allocator, which reports `None` for every reservation.
	Bandwidth() *MoqBandwidth
	// Close the session with the given error code, stopping any reconnect loop.
	Cancel(code uint32)
	// Wait until the session is over.
	//
	// A client session resolves when its connection stops for good: `Err` with the
	// terminal error when it gave up (retries exhausted, or the session's close reason
	// with reconnecting disabled), `Ok` after a local [`shutdown`](Self::shutdown) /
	// [`cancel`](Self::cancel). Transient drops the reconnect loop rides out do not
	// resolve this; watch [`status`](Self::status) for those. A server-accepted
	// session resolves with the session's close reason.
	Closed(
		ctx context.Context) error
	// The subscribe-side origin: a read handle for receiving
	// announcements pushed by the remote. Either derived from the
	// origin the caller wired via `set_consume`, or auto-created if
	// neither was set.
	Consume() *MoqOriginConsumer
	// The connection epoch: 1 for the connect this session was built from, one more
	// on each reconnect. A server-accepted session is a single transport, so it stays 1.
	//
	// The count pairs with [`status`](Self::status): a `Connected` transition whose
	// epoch grew is a reconnect, so a worker can log each one by number. Migrations
	// count too, since the replacement is a new session.
	Epoch() uint64
	// The publish-side origin: where local broadcasts get advertised
	// to the remote. Either the producer the caller wired via
	// `set_publish` / `set_consume` before connect/accept, or one
	// auto-created if neither was set.
	Publish() *MoqOriginProducer
	// Graceful shutdown. Equivalent to `cancel(0)`. Documents the
	// convention that code 0 means "no error" so callers don't have to
	// pick one. Named `shutdown` (not `close`) because UniFFI's Kotlin
	// generator already emits an `AutoCloseable.close()` that releases
	// the FFI handle, and shadowing it would silently mean a different
	// thing per binding.
	Shutdown()
	// Snapshot the current connection statistics (RTT, bandwidth estimates,
	// byte/packet counters). Cheap to call; intended for periodic polling.
	//
	// Individual fields are `None` when the transport backend doesn't report
	// them, or (on a client session) while the connection is between sessions;
	// see [`MoqConnectionStats`].
	Stats() MoqConnectionStats
	// Wait for the connection status to differ from the one this handle last reported.
	//
	// A client session reports `Connected` first (the connect it was built from), then
	// follows the reconnect loop: `Disconnected` while redialing, `Connected` again on
	// success, `Migrating` during a GOAWAY handover. It returns an error once the
	// connection stops for good (same terminal result as [`closed`](Self::closed)).
	// A server-accepted session is a single transport, so its only transition is
	// terminal: this waits for the close and returns its reason.
	//
	// This is the current status, not a queue of every edge: a drop that reconnects
	// before you ask again is coalesced away, so the outages it hides are the ones
	// that already healed. Don't count outages with it.
	Status(
		ctx context.Context) (MoqConnectionStatus, error)
}
type MoqSession struct {
	ffiObject FfiObject
}

// The session's bandwidth allocator, used to divide the connection's send
// estimate among tracks sharing it.
//
// Every call returns a handle to the same registry, so reservations made
// through one are visible to the others. A client handle survives
// reconnects: the grant is `None` while disconnected and resumes on the
// next connection. An accepted session with no congestion estimate mints
// an unlimited allocator, which reports `None` for every reservation.
func (_self *MoqSession) Bandwidth() *MoqBandwidth {
	_pointer := _self.ffiObject.incrementPointer("*MoqSession")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMoqBandwidthINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqsession_bandwidth(
			_pointer, _uniffiStatus)
	}))
}

// Close the session with the given error code, stopping any reconnect loop.
func (_self *MoqSession) Cancel(code uint32) {
	_pointer := _self.ffiObject.incrementPointer("*MoqSession")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqsession_cancel(
			_pointer, FfiConverterUint32INSTANCE.Lower(code), _uniffiStatus)
		return false
	})
}

// Wait until the session is over.
//
// A client session resolves when its connection stops for good: `Err` with the
// terminal error when it gave up (retries exhausted, or the session's close reason
// with reconnecting disabled), `Ok` after a local [`shutdown`](Self::shutdown) /
// [`cancel`](Self::cancel). Transient drops the reconnect loop rides out do not
// resolve this; watch [`status`](Self::status) for those. A server-accepted
// session resolves with the session's close reason.
func (_self *MoqSession) Closed(
	ctx context.Context) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqSession")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqsession_closed(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_void(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	return err
}

// The subscribe-side origin: a read handle for receiving
// announcements pushed by the remote. Either derived from the
// origin the caller wired via `set_consume`, or auto-created if
// neither was set.
func (_self *MoqSession) Consume() *MoqOriginConsumer {
	_pointer := _self.ffiObject.incrementPointer("*MoqSession")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMoqOriginConsumerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqsession_consume(
			_pointer, _uniffiStatus)
	}))
}

// The connection epoch: 1 for the connect this session was built from, one more
// on each reconnect. A server-accepted session is a single transport, so it stays 1.
//
// The count pairs with [`status`](Self::status): a `Connected` transition whose
// epoch grew is a reconnect, so a worker can log each one by number. Migrations
// count too, since the replacement is a new session.
func (_self *MoqSession) Epoch() uint64 {
	_pointer := _self.ffiObject.incrementPointer("*MoqSession")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterUint64INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqsession_epoch(
			_pointer, _uniffiStatus)
	}))
}

// The publish-side origin: where local broadcasts get advertised
// to the remote. Either the producer the caller wired via
// `set_publish` / `set_consume` before connect/accept, or one
// auto-created if neither was set.
func (_self *MoqSession) Publish() *MoqOriginProducer {
	_pointer := _self.ffiObject.incrementPointer("*MoqSession")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMoqOriginProducerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqsession_publish(
			_pointer, _uniffiStatus)
	}))
}

// Graceful shutdown. Equivalent to `cancel(0)`. Documents the
// convention that code 0 means "no error" so callers don't have to
// pick one. Named `shutdown` (not `close`) because UniFFI's Kotlin
// generator already emits an `AutoCloseable.close()` that releases
// the FFI handle, and shadowing it would silently mean a different
// thing per binding.
func (_self *MoqSession) Shutdown() {
	_pointer := _self.ffiObject.incrementPointer("*MoqSession")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqsession_shutdown(
			_pointer, _uniffiStatus)
		return false
	})
}

// Snapshot the current connection statistics (RTT, bandwidth estimates,
// byte/packet counters). Cheap to call; intended for periodic polling.
//
// Individual fields are `None` when the transport backend doesn't report
// them, or (on a client session) while the connection is between sessions;
// see [`MoqConnectionStats`].
func (_self *MoqSession) Stats() MoqConnectionStats {
	_pointer := _self.ffiObject.incrementPointer("*MoqSession")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMoqConnectionStatsINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqsession_stats(
				_pointer, _uniffiStatus),
		}
	}))
}

// Wait for the connection status to differ from the one this handle last reported.
//
// A client session reports `Connected` first (the connect it was built from), then
// follows the reconnect loop: `Disconnected` while redialing, `Connected` again on
// success, `Migrating` during a GOAWAY handover. It returns an error once the
// connection stops for good (same terminal result as [`closed`](Self::closed)).
// A server-accepted session is a single transport, so its only transition is
// terminal: this waits for the close and returns its reason.
//
// This is the current status, not a queue of every edge: a drop that reconnects
// before you ask again is coalesced away, so the outages it hides are the ones
// that already healed. Don't count outages with it.
func (_self *MoqSession) Status(
	ctx context.Context) (MoqConnectionStatus, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqSession")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) MoqConnectionStatus {
			return FfiConverterMoqConnectionStatusINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqsession_status(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_rust_buffer(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	return res, err
}
func (object *MoqSession) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqSession struct{}

var FfiConverterMoqSessionINSTANCE = FfiConverterMoqSession{}

func (c FfiConverterMoqSession) Lift(handle C.uint64_t) *MoqSession {
	result := &MoqSession{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqsession(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqsession(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqSession).Destroy)
	return result
}

func (c FfiConverterMoqSession) Read(reader io.Reader) *MoqSession {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqSession) Lower(value *MoqSession) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqSession")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqSession) Write(writer io.Writer, value *MoqSession) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqSession(handle uint64) *MoqSession {
	return FfiConverterMoqSessionINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqSession(value *MoqSession) uint64 {
	return uint64(FfiConverterMoqSessionINSTANCE.Lower(value))
}

type FfiDestroyerMoqSession struct{}

func (_ FfiDestroyerMoqSession) Destroy(value *MoqSession) {
	value.Destroy()
}

type MoqTrackConsumerInterface interface {
	// Cancel all current and future reads.
	//
	// Terminal: the subscription is released here, not when the handle is.
	Cancel()
	// Return the publisher-side track properties learned during subscription.
	Info() (MoqTrackInfo, error)
	// Return the next group with a higher sequence number than any previously
	// returned, skipping late arrivals. Returns `None` when the track ends.
	//
	// Shares the sequence cursor with [`Self::read_frame`]: a group one method
	// has already taken is not returned by the other. A `read_frame` cancelled
	// after acquiring a group leaves that group here.
	//
	// The first call commits this track to sequence order: arrival-order reads
	// ([`Self::recv_group`]) fail with [`MoqError::AlreadyCommitted`] afterwards.
	NextGroup(
		ctx context.Context) (**MoqGroupConsumer, error)
	// Read the first frame of the next group, including its timestamp.
	//
	// Convenience for tracks using one-frame-per-group (like moq-boy's
	// status/command tracks). Completed empty groups are skipped. Returns `None`
	// only when the track ends. Cancelling one call keeps the current group so a
	// later `read_frame` or [`Self::next_group`] still sees it.
	//
	// Shares the sequence cursor with [`Self::next_group`], committing the track
	// the same way.
	ReadFrame(
		ctx context.Context) (*MoqFrame, error)
	// Receive the next best-effort datagram in arrival order.
	//
	// Returns `None` when the track ends. Datagram delivery is unavailable over
	// pre-lite-05 moq-lite and stream-only transports.
	// Datagrams are a separate cursor from groups, so this works alongside either
	// group order, never commits the track to one, and progresses while a group
	// read is pending.
	RecvDatagram(
		ctx context.Context) (*MoqDatagram, error)
	// Return the next group in arrival order. Returns `None` when the track ends.
	//
	// Groups are returned as they arrive on the wire, which may be out of sequence
	// order (e.g. if a later group lands before an earlier one on a separate stream).
	//
	// The first call commits this track to arrival order: sequence-order reads
	// ([`Self::next_group`], [`Self::read_frame`]) fail with
	// [`MoqError::AlreadyCommitted`] afterwards.
	RecvGroup(
		ctx context.Context) (**MoqGroupConsumer, error)
	// Change this subscriber's delivery preferences.
	//
	// Silently ignored if the track already ended; the update is meaningless at
	// that point.
	Update(subscription MoqSubscription)
}
type MoqTrackConsumer struct {
	ffiObject FfiObject
}

// Cancel all current and future reads.
//
// Terminal: the subscription is released here, not when the handle is.
func (_self *MoqTrackConsumer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqtrackconsumer_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Return the publisher-side track properties learned during subscription.
func (_self *MoqTrackConsumer) Info() (MoqTrackInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackConsumer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqtrackconsumer_info(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue MoqTrackInfo
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackInfoINSTANCE.Lift(_uniffiRV), nil
	}
}

// Return the next group with a higher sequence number than any previously
// returned, skipping late arrivals. Returns `None` when the track ends.
//
// Shares the sequence cursor with [`Self::read_frame`]: a group one method
// has already taken is not returned by the other. A `read_frame` cancelled
// after acquiring a group leaves that group here.
//
// The first call commits this track to sequence order: arrival-order reads
// ([`Self::recv_group`]) fail with [`MoqError::AlreadyCommitted`] afterwards.
func (_self *MoqTrackConsumer) NextGroup(
	ctx context.Context) (**MoqGroupConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) **MoqGroupConsumer {
			return FfiConverterOptionalMoqGroupConsumerINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqtrackconsumer_next_group(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_rust_buffer(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	return res, err
}

// Read the first frame of the next group, including its timestamp.
//
// Convenience for tracks using one-frame-per-group (like moq-boy's
// status/command tracks). Completed empty groups are skipped. Returns `None`
// only when the track ends. Cancelling one call keeps the current group so a
// later `read_frame` or [`Self::next_group`] still sees it.
//
// Shares the sequence cursor with [`Self::next_group`], committing the track
// the same way.
func (_self *MoqTrackConsumer) ReadFrame(
	ctx context.Context) (*MoqFrame, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MoqFrame {
			return FfiConverterOptionalMoqFrameINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqtrackconsumer_read_frame(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_rust_buffer(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	return res, err
}

// Receive the next best-effort datagram in arrival order.
//
// Returns `None` when the track ends. Datagram delivery is unavailable over
// pre-lite-05 moq-lite and stream-only transports.
// Datagrams are a separate cursor from groups, so this works alongside either
// group order, never commits the track to one, and progresses while a group
// read is pending.
func (_self *MoqTrackConsumer) RecvDatagram(
	ctx context.Context) (*MoqDatagram, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MoqDatagram {
			return FfiConverterOptionalMoqDatagramINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqtrackconsumer_recv_datagram(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_rust_buffer(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	return res, err
}

// Return the next group in arrival order. Returns `None` when the track ends.
//
// Groups are returned as they arrive on the wire, which may be out of sequence
// order (e.g. if a later group lands before an earlier one on a separate stream).
//
// The first call commits this track to arrival order: sequence-order reads
// ([`Self::next_group`], [`Self::read_frame`]) fail with
// [`MoqError::AlreadyCommitted`] afterwards.
func (_self *MoqTrackConsumer) RecvGroup(
	ctx context.Context) (**MoqGroupConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) **MoqGroupConsumer {
			return FfiConverterOptionalMoqGroupConsumerINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqtrackconsumer_recv_group(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_rust_buffer(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	return res, err
}

// Change this subscriber's delivery preferences.
//
// Silently ignored if the track already ended; the update is meaningless at
// that point.
func (_self *MoqTrackConsumer) Update(subscription MoqSubscription) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqtrackconsumer_update(
			_pointer, FfiConverterMoqSubscriptionINSTANCE.Lower(subscription), _uniffiStatus)
		return false
	})
}
func (object *MoqTrackConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqTrackConsumer struct{}

var FfiConverterMoqTrackConsumerINSTANCE = FfiConverterMoqTrackConsumer{}

func (c FfiConverterMoqTrackConsumer) Lift(handle C.uint64_t) *MoqTrackConsumer {
	result := &MoqTrackConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqtrackconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqtrackconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqTrackConsumer).Destroy)
	return result
}

func (c FfiConverterMoqTrackConsumer) Read(reader io.Reader) *MoqTrackConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqTrackConsumer) Lower(value *MoqTrackConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqTrackConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqTrackConsumer) Write(writer io.Writer, value *MoqTrackConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqTrackConsumer(handle uint64) *MoqTrackConsumer {
	return FfiConverterMoqTrackConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqTrackConsumer(value *MoqTrackConsumer) uint64 {
	return uint64(FfiConverterMoqTrackConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqTrackConsumer struct{}

func (_ FfiDestroyerMoqTrackConsumer) Destroy(value *MoqTrackConsumer) {
	value.Destroy()
}

// A watch-only handle to a published track's subscriber demand.
//
// Weak: holding it neither keeps the track open nor locks the producer it came from, so a wait
// can park here while the producer keeps publishing. Waits fail with `Closed` once the track is
// released.
type MoqTrackDemandInterface interface {
	// Whether the track has at least one active consumer right now, without waiting.
	IsUsed() bool
	// The name of the track this watches.
	Name() string
	// Wait until the track has no active consumers.
	Unused(
		ctx context.Context) error
	// Wait until the track has at least one active consumer.
	Used(
		ctx context.Context) error
}

// A watch-only handle to a published track's subscriber demand.
//
// Weak: holding it neither keeps the track open nor locks the producer it came from, so a wait
// can park here while the producer keeps publishing. Waits fail with `Closed` once the track is
// released.
type MoqTrackDemand struct {
	ffiObject FfiObject
}

// Whether the track has at least one active consumer right now, without waiting.
func (_self *MoqTrackDemand) IsUsed() bool {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackDemand")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_moq_ffi_fn_method_moqtrackdemand_is_used(
			_pointer, _uniffiStatus)
	}))
}

// The name of the track this watches.
func (_self *MoqTrackDemand) Name() string {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackDemand")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqtrackdemand_name(
				_pointer, _uniffiStatus),
		}
	}))
}

// Wait until the track has no active consumers.
func (_self *MoqTrackDemand) Unused(
	ctx context.Context) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackDemand")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqtrackdemand_unused(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_void(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	return err
}

// Wait until the track has at least one active consumer.
func (_self *MoqTrackDemand) Used(
	ctx context.Context) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackDemand")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqtrackdemand_used(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_void(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	return err
}
func (object *MoqTrackDemand) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqTrackDemand struct{}

var FfiConverterMoqTrackDemandINSTANCE = FfiConverterMoqTrackDemand{}

func (c FfiConverterMoqTrackDemand) Lift(handle C.uint64_t) *MoqTrackDemand {
	result := &MoqTrackDemand{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqtrackdemand(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqtrackdemand(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqTrackDemand).Destroy)
	return result
}

func (c FfiConverterMoqTrackDemand) Read(reader io.Reader) *MoqTrackDemand {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqTrackDemand) Lower(value *MoqTrackDemand) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqTrackDemand")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqTrackDemand) Write(writer io.Writer, value *MoqTrackDemand) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqTrackDemand(handle uint64) *MoqTrackDemand {
	return FfiConverterMoqTrackDemandINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqTrackDemand(value *MoqTrackDemand) uint64 {
	return uint64(FfiConverterMoqTrackDemandINSTANCE.Lower(value))
}

type FfiDestroyerMoqTrackDemand struct{}

func (_ FfiDestroyerMoqTrackDemand) Destroy(value *MoqTrackDemand) {
	value.Destroy()
}

// Serves on-demand fetches of uncached groups for one track.
type MoqTrackDynamicInterface interface {
	// Cancel all current and future `requested_group()` calls.
	//
	// Terminal: the dynamic track is released here, not when the handle is, so any pending
	// fetch is rejected.
	Cancel()
	// Wait for the next fetch of an uncached group.
	//
	// Accept the returned request to produce the group, or abort it with an
	// application error. Cached groups are served without reaching this method.
	RequestedGroup(
		ctx context.Context) (*MoqGroupRequest, error)
}

// Serves on-demand fetches of uncached groups for one track.
type MoqTrackDynamic struct {
	ffiObject FfiObject
}

// Cancel all current and future `requested_group()` calls.
//
// Terminal: the dynamic track is released here, not when the handle is, so any pending
// fetch is rejected.
func (_self *MoqTrackDynamic) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackDynamic")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqtrackdynamic_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Wait for the next fetch of an uncached group.
//
// Accept the returned request to produce the group, or abort it with an
// application error. Cached groups are served without reaching this method.
func (_self *MoqTrackDynamic) RequestedGroup(
	ctx context.Context) (*MoqGroupRequest, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackDynamic")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqGroupRequest {
			return FfiConverterMoqGroupRequestINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqtrackdynamic_requested_group(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_u64(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	return res, err
}
func (object *MoqTrackDynamic) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqTrackDynamic struct{}

var FfiConverterMoqTrackDynamicINSTANCE = FfiConverterMoqTrackDynamic{}

func (c FfiConverterMoqTrackDynamic) Lift(handle C.uint64_t) *MoqTrackDynamic {
	result := &MoqTrackDynamic{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqtrackdynamic(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqtrackdynamic(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqTrackDynamic).Destroy)
	return result
}

func (c FfiConverterMoqTrackDynamic) Read(reader io.Reader) *MoqTrackDynamic {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqTrackDynamic) Lower(value *MoqTrackDynamic) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqTrackDynamic")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqTrackDynamic) Write(writer io.Writer, value *MoqTrackDynamic) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqTrackDynamic(handle uint64) *MoqTrackDynamic {
	return FfiConverterMoqTrackDynamicINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqTrackDynamic(value *MoqTrackDynamic) uint64 {
	return uint64(FfiConverterMoqTrackDynamicINSTANCE.Lower(value))
}

type FfiDestroyerMoqTrackDynamic struct{}

func (_ FfiDestroyerMoqTrackDynamic) Destroy(value *MoqTrackDynamic) {
	value.Destroy()
}

type MoqTrackProducerInterface interface {
	// Abort this track with an application error code.
	Abort(errorCode uint16) error
	// Send `frame` as a best-effort datagram, returning the sequence number assigned to it.
	//
	// The payload must be at most 1200 bytes. Datagrams are only delivered on transports and
	// wire versions with a datagram channel; there is no stream fallback.
	AppendDatagram(frame MoqFrame) (uint64, error)
	// Append a new group to the track, returning a producer for writing frames into it.
	AppendGroup() (*MoqGroupProducer, error)
	// Create a consumer that reads from this producer's track.
	//
	// Useful for local pub/sub without going through an origin/broadcast. `subscription`
	// tunes delivery priority, group range, and staleness; omit for defaults.
	Consume(subscription *MoqSubscription) (*MoqTrackConsumer, error)
	// Create a group with an explicit sequence number.
	//
	// Use this for sparse or replayed tracks. [`append_group`](Self::append_group)
	// remains the convenient live-stream path.
	CreateGroup(sequence uint64) (*MoqGroupProducer, error)
	// A watch-only handle to whether this track has subscribers.
	Demand() (*MoqTrackDemand, error)
	// Create a handler for uncached group fetches on this track.
	//
	// Hold the returned object for as long as cache misses should wait to be
	// served. Without a live dynamic handler, a missing group fails with `NotFound`.
	Dynamic() (*MoqTrackDynamic, error)
	// End the track at the live edge.
	//
	// [`finish_at`](Self::finish_at) declares the boundary ahead of time, so this keeps
	// that boundary. The handle remains so a later [`abort`](Self::abort) can still run.
	Finish() error
	// Declare the exclusive final group sequence, possibly ahead of the live edge.
	//
	// Groups below `final_sequence` may still be created afterwards. Groups at or
	// above it are rejected. The producer remains open for groups below the boundary;
	// call [`finish`](Self::finish) after producing the remaining groups.
	FinishAt(finalSequence uint64) error
	// Return the name of this track.
	Name() (string, error)
	// Wait until this track has no active consumers.
	//
	// Prefer [`demand`](Self::demand), a handle that can wait without borrowing this producer.
	Unused(
		ctx context.Context) error
	// Wait until this track has at least one active consumer.
	//
	// Prefer [`demand`](Self::demand), a handle that can wait without borrowing this producer.
	Used(
		ctx context.Context) error
	// Write `frame` as a single-frame group.
	//
	// Raw tracks default to a microsecond timescale. Custom timescales may round
	// the timestamp during conversion.
	WriteFrame(frame MoqFrame) error
}
type MoqTrackProducer struct {
	ffiObject FfiObject
}

// Abort this track with an application error code.
func (_self *MoqTrackProducer) Abort(errorCode uint16) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqtrackproducer_abort(
			_pointer, FfiConverterUint16INSTANCE.Lower(errorCode), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Send `frame` as a best-effort datagram, returning the sequence number assigned to it.
//
// The payload must be at most 1200 bytes. Datagrams are only delivered on transports and
// wire versions with a datagram channel; there is no stream fallback.
func (_self *MoqTrackProducer) AppendDatagram(frame MoqFrame) (uint64, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqtrackproducer_append_datagram(
			_pointer, FfiConverterMoqFrameINSTANCE.Lower(frame), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue uint64
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterUint64INSTANCE.Lift(_uniffiRV), nil
	}
}

// Append a new group to the track, returning a producer for writing frames into it.
func (_self *MoqTrackProducer) AppendGroup() (*MoqGroupProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqtrackproducer_append_group(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqGroupProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqGroupProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a consumer that reads from this producer's track.
//
// Useful for local pub/sub without going through an origin/broadcast. `subscription`
// tunes delivery priority, group range, and staleness; omit for defaults.
func (_self *MoqTrackProducer) Consume(subscription *MoqSubscription) (*MoqTrackConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqtrackproducer_consume(
			_pointer, FfiConverterOptionalMoqSubscriptionINSTANCE.Lower(subscription), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqTrackConsumer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackConsumerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a group with an explicit sequence number.
//
// Use this for sparse or replayed tracks. [`append_group`](Self::append_group)
// remains the convenient live-stream path.
func (_self *MoqTrackProducer) CreateGroup(sequence uint64) (*MoqGroupProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqtrackproducer_create_group(
			_pointer, FfiConverterUint64INSTANCE.Lower(sequence), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqGroupProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqGroupProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// A watch-only handle to whether this track has subscribers.
func (_self *MoqTrackProducer) Demand() (*MoqTrackDemand, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqtrackproducer_demand(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqTrackDemand
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackDemandINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a handler for uncached group fetches on this track.
//
// Hold the returned object for as long as cache misses should wait to be
// served. Without a live dynamic handler, a missing group fails with `NotFound`.
func (_self *MoqTrackProducer) Dynamic() (*MoqTrackDynamic, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqtrackproducer_dynamic(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqTrackDynamic
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackDynamicINSTANCE.Lift(_uniffiRV), nil
	}
}

// End the track at the live edge.
//
// [`finish_at`](Self::finish_at) declares the boundary ahead of time, so this keeps
// that boundary. The handle remains so a later [`abort`](Self::abort) can still run.
func (_self *MoqTrackProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqtrackproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Declare the exclusive final group sequence, possibly ahead of the live edge.
//
// Groups below `final_sequence` may still be created afterwards. Groups at or
// above it are rejected. The producer remains open for groups below the boundary;
// call [`finish`](Self::finish) after producing the remaining groups.
func (_self *MoqTrackProducer) FinishAt(finalSequence uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqtrackproducer_finish_at(
			_pointer, FfiConverterUint64INSTANCE.Lower(finalSequence), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Return the name of this track.
func (_self *MoqTrackProducer) Name() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqtrackproducer_name(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Wait until this track has no active consumers.
//
// Prefer [`demand`](Self::demand), a handle that can wait without borrowing this producer.
func (_self *MoqTrackProducer) Unused(
	ctx context.Context) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqtrackproducer_unused(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_void(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	return err
}

// Wait until this track has at least one active consumer.
//
// Prefer [`demand`](Self::demand), a handle that can wait without borrowing this producer.
func (_self *MoqTrackProducer) Used(
	ctx context.Context) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqtrackproducer_used(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_void(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	return err
}

// Write `frame` as a single-frame group.
//
// Raw tracks default to a microsecond timescale. Custom timescales may round
// the timestamp during conversion.
func (_self *MoqTrackProducer) WriteFrame(frame MoqFrame) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqtrackproducer_write_frame(
			_pointer, FfiConverterMoqFrameINSTANCE.Lower(frame), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqTrackProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqTrackProducer struct{}

var FfiConverterMoqTrackProducerINSTANCE = FfiConverterMoqTrackProducer{}

func (c FfiConverterMoqTrackProducer) Lift(handle C.uint64_t) *MoqTrackProducer {
	result := &MoqTrackProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqtrackproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqtrackproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqTrackProducer).Destroy)
	return result
}

func (c FfiConverterMoqTrackProducer) Read(reader io.Reader) *MoqTrackProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqTrackProducer) Lower(value *MoqTrackProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqTrackProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqTrackProducer) Write(writer io.Writer, value *MoqTrackProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqTrackProducer(handle uint64) *MoqTrackProducer {
	return FfiConverterMoqTrackProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqTrackProducer(value *MoqTrackProducer) uint64 {
	return uint64(FfiConverterMoqTrackProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqTrackProducer struct{}

func (_ FfiDestroyerMoqTrackProducer) Destroy(value *MoqTrackProducer) {
	value.Destroy()
}

// A track requested by a subscriber that hasn't been accepted yet.
//
// Mirrors [`moq_net::track::Request`]: [`accept`](Self::accept) it to start producing raw
// frames, hand it to [`MoqBroadcastProducer::publish_audio_on_track`] to publish media,
// or [`abort`](Self::abort) it to reject the waiting subscriber.
type MoqTrackRequestInterface interface {
	// Reject the request with an application error code, failing the waiting subscriber.
	Abort(errorCode uint16) error
	// Accept the request as a raw track, fixing its [`MoqTrackInfo`] (timescale, etc.).
	//
	// For media use [`MoqBroadcastProducer::publish_audio_on_track`] instead, which lets
	// the importer pick the timescale.
	Accept(info *MoqTrackInfo) (*MoqTrackProducer, error)
	// Create a handler for uncached group fetches before accepting this track.
	//
	// Obtain and retain this handle before `accept()` when the track itself was
	// requested by a fetch. This keeps the pending group request serviceable across
	// the transition from request to producer.
	Dynamic() (*MoqTrackDynamic, error)
	// The requested track name.
	Name() (string, error)
}

// A track requested by a subscriber that hasn't been accepted yet.
//
// Mirrors [`moq_net::track::Request`]: [`accept`](Self::accept) it to start producing raw
// frames, hand it to [`MoqBroadcastProducer::publish_audio_on_track`] to publish media,
// or [`abort`](Self::abort) it to reject the waiting subscriber.
type MoqTrackRequest struct {
	ffiObject FfiObject
}

// Reject the request with an application error code, failing the waiting subscriber.
func (_self *MoqTrackRequest) Abort(errorCode uint16) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackRequest")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqtrackrequest_abort(
			_pointer, FfiConverterUint16INSTANCE.Lower(errorCode), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Accept the request as a raw track, fixing its [`MoqTrackInfo`] (timescale, etc.).
//
// For media use [`MoqBroadcastProducer::publish_audio_on_track`] instead, which lets
// the importer pick the timescale.
func (_self *MoqTrackRequest) Accept(info *MoqTrackInfo) (*MoqTrackProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackRequest")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqtrackrequest_accept(
			_pointer, FfiConverterOptionalMoqTrackInfoINSTANCE.Lower(info), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqTrackProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a handler for uncached group fetches before accepting this track.
//
// Obtain and retain this handle before `accept()` when the track itself was
// requested by a fetch. This keeps the pending group request serviceable across
// the transition from request to producer.
func (_self *MoqTrackRequest) Dynamic() (*MoqTrackDynamic, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackRequest")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqtrackrequest_dynamic(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqTrackDynamic
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackDynamicINSTANCE.Lift(_uniffiRV), nil
	}
}

// The requested track name.
func (_self *MoqTrackRequest) Name() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackRequest")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqtrackrequest_name(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}
func (object *MoqTrackRequest) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqTrackRequest struct{}

var FfiConverterMoqTrackRequestINSTANCE = FfiConverterMoqTrackRequest{}

func (c FfiConverterMoqTrackRequest) Lift(handle C.uint64_t) *MoqTrackRequest {
	result := &MoqTrackRequest{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqtrackrequest(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqtrackrequest(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqTrackRequest).Destroy)
	return result
}

func (c FfiConverterMoqTrackRequest) Read(reader io.Reader) *MoqTrackRequest {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqTrackRequest) Lower(value *MoqTrackRequest) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqTrackRequest")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqTrackRequest) Write(writer io.Writer, value *MoqTrackRequest) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqTrackRequest(handle uint64) *MoqTrackRequest {
	return FfiConverterMoqTrackRequestINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqTrackRequest(value *MoqTrackRequest) uint64 {
	return uint64(FfiConverterMoqTrackRequestINSTANCE.Lower(value))
}

type FfiDestroyerMoqTrackRequest struct{}

func (_ FfiDestroyerMoqTrackRequest) Destroy(value *MoqTrackRequest) {
	value.Destroy()
}

// Consumer for a video track decoded inside the bindings.
type MoqVideoConsumerInterface interface {
	// Make current and future reads return `Cancelled`.
	//
	// Terminal: the decoder session is released here, not when the handle is.
	Cancel()
	// The next decoded frame, or `None` once the track ends.
	Next(
		ctx context.Context) (*MoqVideoDecodedFrame, error)
}

// Consumer for a video track decoded inside the bindings.
type MoqVideoConsumer struct {
	ffiObject FfiObject
}

// Make current and future reads return `Cancelled`.
//
// Terminal: the decoder session is released here, not when the handle is.
func (_self *MoqVideoConsumer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqVideoConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqvideoconsumer_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// The next decoded frame, or `None` once the track ends.
func (_self *MoqVideoConsumer) Next(
	ctx context.Context) (*MoqVideoDecodedFrame, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqVideoConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MoqVideoDecodedFrame {
			return FfiConverterOptionalMoqVideoDecodedFrameINSTANCE.Lift(ffi)
		},
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqvideoconsumer_next(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_rust_buffer(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	return res, err
}
func (object *MoqVideoConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqVideoConsumer struct{}

var FfiConverterMoqVideoConsumerINSTANCE = FfiConverterMoqVideoConsumer{}

func (c FfiConverterMoqVideoConsumer) Lift(handle C.uint64_t) *MoqVideoConsumer {
	result := &MoqVideoConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqvideoconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqvideoconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqVideoConsumer).Destroy)
	return result
}

func (c FfiConverterMoqVideoConsumer) Read(reader io.Reader) *MoqVideoConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqVideoConsumer) Lower(value *MoqVideoConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqVideoConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqVideoConsumer) Write(writer io.Writer, value *MoqVideoConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqVideoConsumer(handle uint64) *MoqVideoConsumer {
	return FfiConverterMoqVideoConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqVideoConsumer(value *MoqVideoConsumer) uint64 {
	return uint64(FfiConverterMoqVideoConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqVideoConsumer struct{}

func (_ FfiDestroyerMoqVideoConsumer) Destroy(value *MoqVideoConsumer) {
	value.Destroy()
}

// Producer for a raw-video track.
//
// Built via [`MoqBroadcastProducer::publish_video`]. Each
// [`write`](Self::write) accepts a [`MoqVideoFrame`] whose `data` is in the
// pixel format declared by the [`MoqVideoEncoderInput`] passed at publish time.
type MoqVideoProducerInterface interface {
	// Cut a new group at the next written frame.
	//
	// Optional. The encoder already keyframes every
	// [`gop`](MoqVideoEncoderOutput::gop) frames, and each of those cuts a
	// group, so a subscriber can always join without you calling this. Reach for
	// it only to place the boundaries yourself: aligning groups with something
	// the encoder cannot see, such as a scene change, a source switch, or
	// resuming after an idle gap.
	//
	// The next frame is encoded as a keyframe, which closes the open group and
	// starts a new one at it. Calling this repeatedly before that frame arrives
	// cuts once, not several times.
	//
	// Fails when the selected encoder cannot force a keyframe (a V4L2 driver
	// without the control): nothing is queued, and groups keep falling at the
	// configured interval.
	Cut() error
	// A watch-only handle to whether this video track has subscribers.
	Demand() (*MoqTrackDemand, error)
	// Flush any frames the codec is still holding and finalize the track.
	Finish() error
	// Return the name of this video track.
	Name() (string, error)
	// This encoder's bandwidth reservation, if it was published against a
	// [`MoqBandwidth`]. Dropping the handle does not release the claim; the
	// producer holds it until [`finish`](Self::finish).
	Reservation() **MoqReservation
	// Retune the live encoder to `bitrate` bits per second, taking effect from
	// roughly the next frame. No keyframe is forced, so this is cheap enough to
	// drive from a congestion controller.
	//
	// The configured bitrate is a ceiling on some backends (openh264 rejects a
	// raise above the rate it opened at), so set
	// [`bitrate`](MoqVideoEncoderOutput::bitrate) to the highest you will ask for
	// and adapt downwards from there.
	//
	// When this producer was published against a [`MoqBandwidth`], the reservation
	// and follower ceiling move with it, so a later grant cannot retune above this
	// value.
	//
	// Errors if this backend cannot retune while running. That is not fatal: the
	// encoder keeps running at its current rate, so stop adapting rather than
	// stop publishing.
	SetBitrate(bitrate uint64) error
	// Wait until this video track has no active consumers.
	//
	// Prefer [`demand`](Self::demand), a handle that can wait without borrowing this producer.
	Unused(
		ctx context.Context) error
	// Wait until this video track has at least one active consumer.
	//
	// Prefer [`demand`](Self::demand), a handle that can wait without borrowing this producer.
	Used(
		ctx context.Context) error
	// Encode and publish one raw frame.
	//
	// A backend that pipelines publishes an earlier frame's output here, so a
	// call that emits nothing on the wire is normal rather than an error.
	Write(frame MoqVideoFrame) error
}

// Producer for a raw-video track.
//
// Built via [`MoqBroadcastProducer::publish_video`]. Each
// [`write`](Self::write) accepts a [`MoqVideoFrame`] whose `data` is in the
// pixel format declared by the [`MoqVideoEncoderInput`] passed at publish time.
type MoqVideoProducer struct {
	ffiObject FfiObject
}

// Cut a new group at the next written frame.
//
// Optional. The encoder already keyframes every
// [`gop`](MoqVideoEncoderOutput::gop) frames, and each of those cuts a
// group, so a subscriber can always join without you calling this. Reach for
// it only to place the boundaries yourself: aligning groups with something
// the encoder cannot see, such as a scene change, a source switch, or
// resuming after an idle gap.
//
// The next frame is encoded as a keyframe, which closes the open group and
// starts a new one at it. Calling this repeatedly before that frame arrives
// cuts once, not several times.
//
// Fails when the selected encoder cannot force a keyframe (a V4L2 driver
// without the control): nothing is queued, and groups keep falling at the
// configured interval.
func (_self *MoqVideoProducer) Cut() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqVideoProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqvideoproducer_cut(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// A watch-only handle to whether this video track has subscribers.
func (_self *MoqVideoProducer) Demand() (*MoqTrackDemand, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqVideoProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqvideoproducer_demand(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqTrackDemand
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackDemandINSTANCE.Lift(_uniffiRV), nil
	}
}

// Flush any frames the codec is still holding and finalize the track.
func (_self *MoqVideoProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqVideoProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqvideoproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Return the name of this video track.
func (_self *MoqVideoProducer) Name() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqVideoProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqvideoproducer_name(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// This encoder's bandwidth reservation, if it was published against a
// [`MoqBandwidth`]. Dropping the handle does not release the claim; the
// producer holds it until [`finish`](Self::finish).
func (_self *MoqVideoProducer) Reservation() **MoqReservation {
	_pointer := _self.ffiObject.incrementPointer("*MoqVideoProducer")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalMoqReservationINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqvideoproducer_reservation(
				_pointer, _uniffiStatus),
		}
	}))
}

// Retune the live encoder to `bitrate` bits per second, taking effect from
// roughly the next frame. No keyframe is forced, so this is cheap enough to
// drive from a congestion controller.
//
// The configured bitrate is a ceiling on some backends (openh264 rejects a
// raise above the rate it opened at), so set
// [`bitrate`](MoqVideoEncoderOutput::bitrate) to the highest you will ask for
// and adapt downwards from there.
//
// When this producer was published against a [`MoqBandwidth`], the reservation
// and follower ceiling move with it, so a later grant cannot retune above this
// value.
//
// Errors if this backend cannot retune while running. That is not fatal: the
// encoder keeps running at its current rate, so stop adapting rather than
// stop publishing.
func (_self *MoqVideoProducer) SetBitrate(bitrate uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqVideoProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqvideoproducer_set_bitrate(
			_pointer, FfiConverterUint64INSTANCE.Lower(bitrate), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Wait until this video track has no active consumers.
//
// Prefer [`demand`](Self::demand), a handle that can wait without borrowing this producer.
func (_self *MoqVideoProducer) Unused(
	ctx context.Context) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqVideoProducer")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqvideoproducer_unused(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_void(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	return err
}

// Wait until this video track has at least one active consumer.
//
// Prefer [`demand`](Self::demand), a handle that can wait without borrowing this producer.
func (_self *MoqVideoProducer) Used(
	ctx context.Context) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqVideoProducer")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		ctx,
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		// rustFutureFunc
		func() C.uint64_t {
			return C.uniffi_moq_ffi_fn_method_moqvideoproducer_used(
				_pointer)
		},
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// cancelFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_cancel_void(handle)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	return err
}

// Encode and publish one raw frame.
//
// A backend that pipelines publishes an earlier frame's output here, so a
// call that emits nothing on the wire is normal rather than an error.
func (_self *MoqVideoProducer) Write(frame MoqVideoFrame) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqVideoProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqvideoproducer_write(
			_pointer, FfiConverterMoqVideoFrameINSTANCE.Lower(frame), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqVideoProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqVideoProducer struct{}

var FfiConverterMoqVideoProducerINSTANCE = FfiConverterMoqVideoProducer{}

func (c FfiConverterMoqVideoProducer) Lift(handle C.uint64_t) *MoqVideoProducer {
	result := &MoqVideoProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqvideoproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqvideoproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqVideoProducer).Destroy)
	return result
}

func (c FfiConverterMoqVideoProducer) Read(reader io.Reader) *MoqVideoProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqVideoProducer) Lower(value *MoqVideoProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqVideoProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqVideoProducer) Write(writer io.Writer, value *MoqVideoProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqVideoProducer(handle uint64) *MoqVideoProducer {
	return FfiConverterMoqVideoProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqVideoProducer(value *MoqVideoProducer) uint64 {
	return uint64(FfiConverterMoqVideoProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqVideoProducer struct{}

func (_ FfiDestroyerMoqVideoProducer) Destroy(value *MoqVideoProducer) {
	value.Destroy()
}

// Scope for an announcement stream.
type MoqAnnounceConfig struct {
	// Literal path prefix beneath the origin.
	Prefix string
	// Pattern relative to `prefix`, or `None` for every path beneath it.
	Filter *string
	// Also list hidden paths: those with a segment starting with `.` below the prefix.
	Hidden bool
}

func (r *MoqAnnounceConfig) Destroy() {
	FfiDestroyerString{}.Destroy(r.Prefix)
	FfiDestroyerOptionalString{}.Destroy(r.Filter)
	FfiDestroyerBool{}.Destroy(r.Hidden)
}

type FfiConverterMoqAnnounceConfig struct{}

var FfiConverterMoqAnnounceConfigINSTANCE = FfiConverterMoqAnnounceConfig{}

func (c FfiConverterMoqAnnounceConfig) Lift(rb RustBufferI) MoqAnnounceConfig {
	return LiftFromRustBuffer[MoqAnnounceConfig](c, rb)
}

func (c FfiConverterMoqAnnounceConfig) Read(reader io.Reader) MoqAnnounceConfig {
	return MoqAnnounceConfig{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqAnnounceConfig) Lower(value MoqAnnounceConfig) C.RustBuffer {
	return LowerIntoRustBuffer[MoqAnnounceConfig](c, value)
}

func (c FfiConverterMoqAnnounceConfig) LowerExternal(value MoqAnnounceConfig) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqAnnounceConfig](c, value))
}

func (c FfiConverterMoqAnnounceConfig) Write(writer io.Writer, value MoqAnnounceConfig) {
	FfiConverterStringINSTANCE.Write(writer, value.Prefix)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Filter)
	FfiConverterBoolINSTANCE.Write(writer, value.Hidden)
}

type FfiDestroyerMoqAnnounceConfig struct{}

func (_ FfiDestroyerMoqAnnounceConfig) Destroy(value MoqAnnounceConfig) {
	value.Destroy()
}

type MoqAudio struct {
	// Human-readable rendition name for track pickers.
	Label *string
	// The broadcast serving this rendition's track, relative to the catalog's own broadcast
	// (e.g. `./source`). Absent or empty means the catalog's broadcast.
	Broadcast    *string
	Codec        string
	Description  *[]byte
	SampleRate   uint32
	ChannelCount uint32
	Bitrate      *uint64
	Container    MoqContainer
}

func (r *MoqAudio) Destroy() {
	FfiDestroyerOptionalString{}.Destroy(r.Label)
	FfiDestroyerOptionalString{}.Destroy(r.Broadcast)
	FfiDestroyerString{}.Destroy(r.Codec)
	FfiDestroyerOptionalBytes{}.Destroy(r.Description)
	FfiDestroyerUint32{}.Destroy(r.SampleRate)
	FfiDestroyerUint32{}.Destroy(r.ChannelCount)
	FfiDestroyerOptionalUint64{}.Destroy(r.Bitrate)
	FfiDestroyerMoqContainer{}.Destroy(r.Container)
}

type FfiConverterMoqAudio struct{}

var FfiConverterMoqAudioINSTANCE = FfiConverterMoqAudio{}

func (c FfiConverterMoqAudio) Lift(rb RustBufferI) MoqAudio {
	return LiftFromRustBuffer[MoqAudio](c, rb)
}

func (c FfiConverterMoqAudio) Read(reader io.Reader) MoqAudio {
	return MoqAudio{
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalBytesINSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterMoqContainerINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqAudio) Lower(value MoqAudio) C.RustBuffer {
	return LowerIntoRustBuffer[MoqAudio](c, value)
}

func (c FfiConverterMoqAudio) LowerExternal(value MoqAudio) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqAudio](c, value))
}

func (c FfiConverterMoqAudio) Write(writer io.Writer, value MoqAudio) {
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Label)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Broadcast)
	FfiConverterStringINSTANCE.Write(writer, value.Codec)
	FfiConverterOptionalBytesINSTANCE.Write(writer, value.Description)
	FfiConverterUint32INSTANCE.Write(writer, value.SampleRate)
	FfiConverterUint32INSTANCE.Write(writer, value.ChannelCount)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.Bitrate)
	FfiConverterMoqContainerINSTANCE.Write(writer, value.Container)
}

type FfiDestroyerMoqAudio struct{}

func (_ FfiDestroyerMoqAudio) Destroy(value MoqAudio) {
	value.Destroy()
}

// PCM layout the caller wants out of [`MoqAudioConsumer::next`].
type MoqAudioDecoderOutput struct {
	Format MoqAudioSampleFormat
	// `None` delivers samples at the codec's native rate.
	SampleRate *uint32
	// `None` delivers samples at the codec's native channel count. A count
	// names its layout as [`MoqAudioEncoderInput::channels`] describes, and
	// the decoder remixes to it.
	Channels *uint32
	// Upper bound on buffering before skipping a stalled group, in
	// microseconds. Same congestion-control knob as
	// [`MoqSubscription::max_age_us`](crate::consumer::MoqSubscription::max_age_us):
	// when a group stalls and a newer group is more than this far ahead,
	// the consumer skips. `None` keeps the moq-mux default of zero (skip
	// aggressively). Named `_max` to leave room for a future
	// `min_buffer_us` (jitter-buffer floor), which is a distinct knob: this
	// one bounds how stale a group may be, that one how much to hold before
	// presenting.
	MaxAgeUs *uint64
}

func (r *MoqAudioDecoderOutput) Destroy() {
	FfiDestroyerMoqAudioSampleFormat{}.Destroy(r.Format)
	FfiDestroyerOptionalUint32{}.Destroy(r.SampleRate)
	FfiDestroyerOptionalUint32{}.Destroy(r.Channels)
	FfiDestroyerOptionalUint64{}.Destroy(r.MaxAgeUs)
}

type FfiConverterMoqAudioDecoderOutput struct{}

var FfiConverterMoqAudioDecoderOutputINSTANCE = FfiConverterMoqAudioDecoderOutput{}

func (c FfiConverterMoqAudioDecoderOutput) Lift(rb RustBufferI) MoqAudioDecoderOutput {
	return LiftFromRustBuffer[MoqAudioDecoderOutput](c, rb)
}

func (c FfiConverterMoqAudioDecoderOutput) Read(reader io.Reader) MoqAudioDecoderOutput {
	return MoqAudioDecoderOutput{
		FfiConverterMoqAudioSampleFormatINSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqAudioDecoderOutput) Lower(value MoqAudioDecoderOutput) C.RustBuffer {
	return LowerIntoRustBuffer[MoqAudioDecoderOutput](c, value)
}

func (c FfiConverterMoqAudioDecoderOutput) LowerExternal(value MoqAudioDecoderOutput) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqAudioDecoderOutput](c, value))
}

func (c FfiConverterMoqAudioDecoderOutput) Write(writer io.Writer, value MoqAudioDecoderOutput) {
	FfiConverterMoqAudioSampleFormatINSTANCE.Write(writer, value.Format)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.SampleRate)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.Channels)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.MaxAgeUs)
}

type FfiDestroyerMoqAudioDecoderOutput struct{}

func (_ FfiDestroyerMoqAudioDecoderOutput) Destroy(value MoqAudioDecoderOutput) {
	value.Destroy()
}

// PCM layout the caller will pass to [`MoqAudioProducer::write`].
type MoqAudioEncoderInput struct {
	Format     MoqAudioSampleFormat
	SampleRate uint32
	// Interleaved channel count, which also names the speaker layout by the
	// WAVE convention: 1 mono, 2 stereo, 3 2.1, 4 quad, 5 5.0, 6 5.1, 7 6.1,
	// 8 7.1, in front left, front right, center, LFE, back, side order.
	Channels uint32
}

func (r *MoqAudioEncoderInput) Destroy() {
	FfiDestroyerMoqAudioSampleFormat{}.Destroy(r.Format)
	FfiDestroyerUint32{}.Destroy(r.SampleRate)
	FfiDestroyerUint32{}.Destroy(r.Channels)
}

type FfiConverterMoqAudioEncoderInput struct{}

var FfiConverterMoqAudioEncoderInputINSTANCE = FfiConverterMoqAudioEncoderInput{}

func (c FfiConverterMoqAudioEncoderInput) Lift(rb RustBufferI) MoqAudioEncoderInput {
	return LiftFromRustBuffer[MoqAudioEncoderInput](c, rb)
}

func (c FfiConverterMoqAudioEncoderInput) Read(reader io.Reader) MoqAudioEncoderInput {
	return MoqAudioEncoderInput{
		FfiConverterMoqAudioSampleFormatINSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqAudioEncoderInput) Lower(value MoqAudioEncoderInput) C.RustBuffer {
	return LowerIntoRustBuffer[MoqAudioEncoderInput](c, value)
}

func (c FfiConverterMoqAudioEncoderInput) LowerExternal(value MoqAudioEncoderInput) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqAudioEncoderInput](c, value))
}

func (c FfiConverterMoqAudioEncoderInput) Write(writer io.Writer, value MoqAudioEncoderInput) {
	FfiConverterMoqAudioSampleFormatINSTANCE.Write(writer, value.Format)
	FfiConverterUint32INSTANCE.Write(writer, value.SampleRate)
	FfiConverterUint32INSTANCE.Write(writer, value.Channels)
}

type FfiDestroyerMoqAudioEncoderInput struct{}

func (_ FfiDestroyerMoqAudioEncoderInput) Destroy(value MoqAudioEncoderInput) {
	value.Destroy()
}

// Codec-side configuration. `sample_rate` / `channels` `None` means
// "match the input (snapping the rate up to a libopus-supported
// value if necessary)".
type MoqAudioEncoderOutput struct {
	Codec      *MoqAudioCodec
	SampleRate *uint32
	Channels   *uint32
	Bitrate    *uint32
	// Encoded frame duration in microseconds. Opus accepts exactly
	// 2500/5000/10000/20000/40000/60000 us, and the default 20 ms matches the
	// JS publish path. 0 takes the codec's own frame, which AAC needs.
	FrameDurationUs uint32
}

func (r *MoqAudioEncoderOutput) Destroy() {
	FfiDestroyerMoqAudioCodec{}.Destroy(r.Codec)
	FfiDestroyerOptionalUint32{}.Destroy(r.SampleRate)
	FfiDestroyerOptionalUint32{}.Destroy(r.Channels)
	FfiDestroyerOptionalUint32{}.Destroy(r.Bitrate)
	FfiDestroyerUint32{}.Destroy(r.FrameDurationUs)
}

type FfiConverterMoqAudioEncoderOutput struct{}

var FfiConverterMoqAudioEncoderOutputINSTANCE = FfiConverterMoqAudioEncoderOutput{}

func (c FfiConverterMoqAudioEncoderOutput) Lift(rb RustBufferI) MoqAudioEncoderOutput {
	return LiftFromRustBuffer[MoqAudioEncoderOutput](c, rb)
}

func (c FfiConverterMoqAudioEncoderOutput) Read(reader io.Reader) MoqAudioEncoderOutput {
	return MoqAudioEncoderOutput{
		FfiConverterMoqAudioCodecINSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqAudioEncoderOutput) Lower(value MoqAudioEncoderOutput) C.RustBuffer {
	return LowerIntoRustBuffer[MoqAudioEncoderOutput](c, value)
}

func (c FfiConverterMoqAudioEncoderOutput) LowerExternal(value MoqAudioEncoderOutput) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqAudioEncoderOutput](c, value))
}

func (c FfiConverterMoqAudioEncoderOutput) Write(writer io.Writer, value MoqAudioEncoderOutput) {
	FfiConverterMoqAudioCodecINSTANCE.Write(writer, value.Codec)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.SampleRate)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.Channels)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.Bitrate)
	FfiConverterUint32INSTANCE.Write(writer, value.FrameDurationUs)
}

type FfiDestroyerMoqAudioEncoderOutput struct{}

func (_ FfiDestroyerMoqAudioEncoderOutput) Destroy(value MoqAudioEncoderOutput) {
	value.Destroy()
}

// One audio frame: payload bytes plus a presentation timestamp.
//
// PCM layout is fixed by the producer / consumer config, so it is
// **not** carried per-frame. On the producer side `data` is raw PCM
// in the configured `input_format`; on the consumer side it is raw
// PCM in the configured `output_format`.
type MoqAudioFrame struct {
	// Presentation timestamp of the first sample, in microseconds.
	TimestampUs uint64
	// The samples, in the configured PCM layout.
	Data []byte
}

func (r *MoqAudioFrame) Destroy() {
	FfiDestroyerUint64{}.Destroy(r.TimestampUs)
	FfiDestroyerBytes{}.Destroy(r.Data)
}

type FfiConverterMoqAudioFrame struct{}

var FfiConverterMoqAudioFrameINSTANCE = FfiConverterMoqAudioFrame{}

func (c FfiConverterMoqAudioFrame) Lift(rb RustBufferI) MoqAudioFrame {
	return LiftFromRustBuffer[MoqAudioFrame](c, rb)
}

func (c FfiConverterMoqAudioFrame) Read(reader io.Reader) MoqAudioFrame {
	return MoqAudioFrame{
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterBytesINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqAudioFrame) Lower(value MoqAudioFrame) C.RustBuffer {
	return LowerIntoRustBuffer[MoqAudioFrame](c, value)
}

func (c FfiConverterMoqAudioFrame) LowerExternal(value MoqAudioFrame) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqAudioFrame](c, value))
}

func (c FfiConverterMoqAudioFrame) Write(writer io.Writer, value MoqAudioFrame) {
	FfiConverterUint64INSTANCE.Write(writer, value.TimestampUs)
	FfiConverterBytesINSTANCE.Write(writer, value.Data)
}

type FfiDestroyerMoqAudioFrame struct{}

func (_ FfiDestroyerMoqAudioFrame) Destroy(value MoqAudioFrame) {
	value.Destroy()
}

// What an audio publish needs: a format, its codec init bytes, and an optional label.
//
// `data` is required: an audio importer cannot resolve its config from frames, so it needs the
// OpusHead / AudioSpecificConfig / STREAMINFO up front.
type MoqAudioInit struct {
	// The audio codec.
	Format MoqAudioFormat
	// Codec init bytes. Required: audio has no in-band config.
	Data []byte
	// Human-readable rendition name for a track picker.
	Label *string
	// Track name. `None` derives a unique name from the format. Refused on a requested track,
	// which already carries its name.
	Track *string
}

func (r *MoqAudioInit) Destroy() {
	FfiDestroyerMoqAudioFormat{}.Destroy(r.Format)
	FfiDestroyerBytes{}.Destroy(r.Data)
	FfiDestroyerOptionalString{}.Destroy(r.Label)
	FfiDestroyerOptionalString{}.Destroy(r.Track)
}

type FfiConverterMoqAudioInit struct{}

var FfiConverterMoqAudioInitINSTANCE = FfiConverterMoqAudioInit{}

func (c FfiConverterMoqAudioInit) Lift(rb RustBufferI) MoqAudioInit {
	return LiftFromRustBuffer[MoqAudioInit](c, rb)
}

func (c FfiConverterMoqAudioInit) Read(reader io.Reader) MoqAudioInit {
	return MoqAudioInit{
		FfiConverterMoqAudioFormatINSTANCE.Read(reader),
		FfiConverterBytesINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqAudioInit) Lower(value MoqAudioInit) C.RustBuffer {
	return LowerIntoRustBuffer[MoqAudioInit](c, value)
}

func (c FfiConverterMoqAudioInit) LowerExternal(value MoqAudioInit) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqAudioInit](c, value))
}

func (c FfiConverterMoqAudioInit) Write(writer io.Writer, value MoqAudioInit) {
	FfiConverterMoqAudioFormatINSTANCE.Write(writer, value.Format)
	FfiConverterBytesINSTANCE.Write(writer, value.Data)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Label)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Track)
}

type FfiDestroyerMoqAudioInit struct{}

func (_ FfiDestroyerMoqAudioInit) Destroy(value MoqAudioInit) {
	value.Destroy()
}

// Retry pacing for the automatic reconnect (see [`MoqClient::set_backoff`]).
//
// The delay starts at `initial_us`, multiplies by `multiplier` after each failed
// attempt, and caps at `max_us`. After `timeout_us` of consecutive failures the
// connection gives up for good (0 retries forever); the window resets whenever a
// session stays up past `initial_us`. The defaults mirror the native
// [`moq_tokio::Backoff`]: 1s, x2, 5s, and a 10s window.
type MoqBackoff struct {
	// Delay before the first reconnect attempt, in microseconds.
	InitialUs uint64
	// Multiplier applied to the delay after each failure.
	Multiplier uint32
	// Maximum delay between reconnect attempts, in microseconds.
	MaxUs uint64
	// Time spent retrying before giving up, in microseconds. 0 retries forever.
	TimeoutUs uint64
}

func (r *MoqBackoff) Destroy() {
	FfiDestroyerUint64{}.Destroy(r.InitialUs)
	FfiDestroyerUint32{}.Destroy(r.Multiplier)
	FfiDestroyerUint64{}.Destroy(r.MaxUs)
	FfiDestroyerUint64{}.Destroy(r.TimeoutUs)
}

type FfiConverterMoqBackoff struct{}

var FfiConverterMoqBackoffINSTANCE = FfiConverterMoqBackoff{}

func (c FfiConverterMoqBackoff) Lift(rb RustBufferI) MoqBackoff {
	return LiftFromRustBuffer[MoqBackoff](c, rb)
}

func (c FfiConverterMoqBackoff) Read(reader io.Reader) MoqBackoff {
	return MoqBackoff{
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqBackoff) Lower(value MoqBackoff) C.RustBuffer {
	return LowerIntoRustBuffer[MoqBackoff](c, value)
}

func (c FfiConverterMoqBackoff) LowerExternal(value MoqBackoff) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqBackoff](c, value))
}

func (c FfiConverterMoqBackoff) Write(writer io.Writer, value MoqBackoff) {
	FfiConverterUint64INSTANCE.Write(writer, value.InitialUs)
	FfiConverterUint32INSTANCE.Write(writer, value.Multiplier)
	FfiConverterUint64INSTANCE.Write(writer, value.MaxUs)
	FfiConverterUint64INSTANCE.Write(writer, value.TimeoutUs)
}

type FfiDestroyerMoqBackoff struct{}

func (_ FfiDestroyerMoqBackoff) Destroy(value MoqBackoff) {
	value.Destroy()
}

// Options for a binary data track, in either mode (the mode is fixed by the constructor).
type MoqBinaryConfig struct {
	// DEFLATE-compress each payload, advertised in the catalog entry.
	Compression bool
	// The payloads' media type (e.g. `image/jpeg`), or `None` to leave it unstated.
	Mime *string
}

func (r *MoqBinaryConfig) Destroy() {
	FfiDestroyerBool{}.Destroy(r.Compression)
	FfiDestroyerOptionalString{}.Destroy(r.Mime)
}

type FfiConverterMoqBinaryConfig struct{}

var FfiConverterMoqBinaryConfigINSTANCE = FfiConverterMoqBinaryConfig{}

func (c FfiConverterMoqBinaryConfig) Lift(rb RustBufferI) MoqBinaryConfig {
	return LiftFromRustBuffer[MoqBinaryConfig](c, rb)
}

func (c FfiConverterMoqBinaryConfig) Read(reader io.Reader) MoqBinaryConfig {
	return MoqBinaryConfig{
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqBinaryConfig) Lower(value MoqBinaryConfig) C.RustBuffer {
	return LowerIntoRustBuffer[MoqBinaryConfig](c, value)
}

func (c FfiConverterMoqBinaryConfig) LowerExternal(value MoqBinaryConfig) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqBinaryConfig](c, value))
}

func (c FfiConverterMoqBinaryConfig) Write(writer io.Writer, value MoqBinaryConfig) {
	FfiConverterBoolINSTANCE.Write(writer, value.Compression)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Mime)
}

type FfiDestroyerMoqBinaryConfig struct{}

func (_ FfiDestroyerMoqBinaryConfig) Destroy(value MoqBinaryConfig) {
	value.Destroy()
}

type MoqCatalog struct {
	Video    map[string]MoqVideo
	Audio    map[string]MoqAudio
	Display  *MoqDimensions
	Rotation *float64
	Flip     *bool
	// Untyped application catalog sections, keyed by section name, each value a JSON string.
	// These are the top-level catalog keys beyond `video`/`audio`, carried through verbatim
	// (parse the JSON yourself). Set them on the publish side with
	// [`set_catalog_section`](crate::producer::MoqBroadcastProducer::set_catalog_section).
	Sections map[string]string
}

func (r *MoqCatalog) Destroy() {
	FfiDestroyerMapStringMoqVideo{}.Destroy(r.Video)
	FfiDestroyerMapStringMoqAudio{}.Destroy(r.Audio)
	FfiDestroyerOptionalMoqDimensions{}.Destroy(r.Display)
	FfiDestroyerOptionalFloat64{}.Destroy(r.Rotation)
	FfiDestroyerOptionalBool{}.Destroy(r.Flip)
	FfiDestroyerMapStringString{}.Destroy(r.Sections)
}

type FfiConverterMoqCatalog struct{}

var FfiConverterMoqCatalogINSTANCE = FfiConverterMoqCatalog{}

func (c FfiConverterMoqCatalog) Lift(rb RustBufferI) MoqCatalog {
	return LiftFromRustBuffer[MoqCatalog](c, rb)
}

func (c FfiConverterMoqCatalog) Read(reader io.Reader) MoqCatalog {
	return MoqCatalog{
		FfiConverterMapStringMoqVideoINSTANCE.Read(reader),
		FfiConverterMapStringMoqAudioINSTANCE.Read(reader),
		FfiConverterOptionalMoqDimensionsINSTANCE.Read(reader),
		FfiConverterOptionalFloat64INSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
		FfiConverterMapStringStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqCatalog) Lower(value MoqCatalog) C.RustBuffer {
	return LowerIntoRustBuffer[MoqCatalog](c, value)
}

func (c FfiConverterMoqCatalog) LowerExternal(value MoqCatalog) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqCatalog](c, value))
}

func (c FfiConverterMoqCatalog) Write(writer io.Writer, value MoqCatalog) {
	FfiConverterMapStringMoqVideoINSTANCE.Write(writer, value.Video)
	FfiConverterMapStringMoqAudioINSTANCE.Write(writer, value.Audio)
	FfiConverterOptionalMoqDimensionsINSTANCE.Write(writer, value.Display)
	FfiConverterOptionalFloat64INSTANCE.Write(writer, value.Rotation)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.Flip)
	FfiConverterMapStringStringINSTANCE.Write(writer, value.Sections)
}

type FfiDestroyerMoqCatalog struct{}

func (_ FfiDestroyerMoqCatalog) Destroy(value MoqCatalog) {
	value.Destroy()
}

// A snapshot of connection statistics for a [`MoqSession`].
//
// Each field is `None` when the transport backend doesn't report that metric (native QUIC
// reports all of them; the browser WebTransport reports few or none), or when it isn't yet
// available (e.g. `estimated_send_rate_bps` before the congestion controller has a window). A `None` is
// not the same as a zero value.
type MoqConnectionStats struct {
	// Smoothed round-trip time, in microseconds.
	RttUs *uint64
	// Estimated send bandwidth from the congestion controller, in bits per second.
	EstimatedSendRateBps *uint64
	// Estimated receive bandwidth from MoQ PROBE, in bits per second.
	EstimatedRecvRateBps *uint64
	// Total bytes sent, including retransmissions and overhead.
	BytesSent *uint64
	// Total bytes received, including duplicates and overhead.
	BytesReceived *uint64
	// Total bytes lost (detected via retransmission or acknowledgement).
	BytesLost *uint64
	// Total datagrams sent.
	PacketsSent *uint64
	// Total datagrams received.
	PacketsReceived *uint64
	// Total datagrams detected as lost.
	PacketsLost *uint64
}

func (r *MoqConnectionStats) Destroy() {
	FfiDestroyerOptionalUint64{}.Destroy(r.RttUs)
	FfiDestroyerOptionalUint64{}.Destroy(r.EstimatedSendRateBps)
	FfiDestroyerOptionalUint64{}.Destroy(r.EstimatedRecvRateBps)
	FfiDestroyerOptionalUint64{}.Destroy(r.BytesSent)
	FfiDestroyerOptionalUint64{}.Destroy(r.BytesReceived)
	FfiDestroyerOptionalUint64{}.Destroy(r.BytesLost)
	FfiDestroyerOptionalUint64{}.Destroy(r.PacketsSent)
	FfiDestroyerOptionalUint64{}.Destroy(r.PacketsReceived)
	FfiDestroyerOptionalUint64{}.Destroy(r.PacketsLost)
}

type FfiConverterMoqConnectionStats struct{}

var FfiConverterMoqConnectionStatsINSTANCE = FfiConverterMoqConnectionStats{}

func (c FfiConverterMoqConnectionStats) Lift(rb RustBufferI) MoqConnectionStats {
	return LiftFromRustBuffer[MoqConnectionStats](c, rb)
}

func (c FfiConverterMoqConnectionStats) Read(reader io.Reader) MoqConnectionStats {
	return MoqConnectionStats{
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqConnectionStats) Lower(value MoqConnectionStats) C.RustBuffer {
	return LowerIntoRustBuffer[MoqConnectionStats](c, value)
}

func (c FfiConverterMoqConnectionStats) LowerExternal(value MoqConnectionStats) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqConnectionStats](c, value))
}

func (c FfiConverterMoqConnectionStats) Write(writer io.Writer, value MoqConnectionStats) {
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.RttUs)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.EstimatedSendRateBps)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.EstimatedRecvRateBps)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.BytesSent)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.BytesReceived)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.BytesLost)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.PacketsSent)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.PacketsReceived)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.PacketsLost)
}

type FfiDestroyerMoqConnectionStats struct{}

func (_ FfiDestroyerMoqConnectionStats) Destroy(value MoqConnectionStats) {
	value.Destroy()
}

// What a container publish needs: a format and its leading bytes.
//
// A container publishes and describes its own tracks, so there is no label or hint here: a
// rendition field would have no single track to land on.
type MoqContainerInit struct {
	// The container format.
	Format MoqContainerFormat
	// The leading chunk of the container, decoded immediately. May be empty.
	Data []byte
}

func (r *MoqContainerInit) Destroy() {
	FfiDestroyerMoqContainerFormat{}.Destroy(r.Format)
	FfiDestroyerBytes{}.Destroy(r.Data)
}

type FfiConverterMoqContainerInit struct{}

var FfiConverterMoqContainerInitINSTANCE = FfiConverterMoqContainerInit{}

func (c FfiConverterMoqContainerInit) Lift(rb RustBufferI) MoqContainerInit {
	return LiftFromRustBuffer[MoqContainerInit](c, rb)
}

func (c FfiConverterMoqContainerInit) Read(reader io.Reader) MoqContainerInit {
	return MoqContainerInit{
		FfiConverterMoqContainerFormatINSTANCE.Read(reader),
		FfiConverterBytesINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqContainerInit) Lower(value MoqContainerInit) C.RustBuffer {
	return LowerIntoRustBuffer[MoqContainerInit](c, value)
}

func (c FfiConverterMoqContainerInit) LowerExternal(value MoqContainerInit) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqContainerInit](c, value))
}

func (c FfiConverterMoqContainerInit) Write(writer io.Writer, value MoqContainerInit) {
	FfiConverterMoqContainerFormatINSTANCE.Write(writer, value.Format)
	FfiConverterBytesINSTANCE.Write(writer, value.Data)
}

type FfiDestroyerMoqContainerInit struct{}

func (_ FfiDestroyerMoqContainerInit) Destroy(value MoqContainerInit) {
	value.Destroy()
}

// A best-effort raw track datagram, as received.
//
// Send one with [`append_datagram`](crate::producer::MoqTrackProducer::append_datagram), which
// takes a [`MoqFrame`] and assigns the sequence number for you.
type MoqDatagram struct {
	// Per-track sequence number, shared with groups.
	Sequence uint64
	// Presentation timestamp in microseconds.
	TimestampUs uint64
	// Datagram payload, capped at 1200 bytes.
	Payload []byte
}

func (r *MoqDatagram) Destroy() {
	FfiDestroyerUint64{}.Destroy(r.Sequence)
	FfiDestroyerUint64{}.Destroy(r.TimestampUs)
	FfiDestroyerBytes{}.Destroy(r.Payload)
}

type FfiConverterMoqDatagram struct{}

var FfiConverterMoqDatagramINSTANCE = FfiConverterMoqDatagram{}

func (c FfiConverterMoqDatagram) Lift(rb RustBufferI) MoqDatagram {
	return LiftFromRustBuffer[MoqDatagram](c, rb)
}

func (c FfiConverterMoqDatagram) Read(reader io.Reader) MoqDatagram {
	return MoqDatagram{
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterBytesINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqDatagram) Lower(value MoqDatagram) C.RustBuffer {
	return LowerIntoRustBuffer[MoqDatagram](c, value)
}

func (c FfiConverterMoqDatagram) LowerExternal(value MoqDatagram) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqDatagram](c, value))
}

func (c FfiConverterMoqDatagram) Write(writer io.Writer, value MoqDatagram) {
	FfiConverterUint64INSTANCE.Write(writer, value.Sequence)
	FfiConverterUint64INSTANCE.Write(writer, value.TimestampUs)
	FfiConverterBytesINSTANCE.Write(writer, value.Payload)
}

type FfiDestroyerMoqDatagram struct{}

func (_ FfiDestroyerMoqDatagram) Destroy(value MoqDatagram) {
	value.Destroy()
}

type MoqDimensions struct {
	Width  uint32
	Height uint32
}

func (r *MoqDimensions) Destroy() {
	FfiDestroyerUint32{}.Destroy(r.Width)
	FfiDestroyerUint32{}.Destroy(r.Height)
}

type FfiConverterMoqDimensions struct{}

var FfiConverterMoqDimensionsINSTANCE = FfiConverterMoqDimensions{}

func (c FfiConverterMoqDimensions) Lift(rb RustBufferI) MoqDimensions {
	return LiftFromRustBuffer[MoqDimensions](c, rb)
}

func (c FfiConverterMoqDimensions) Read(reader io.Reader) MoqDimensions {
	return MoqDimensions{
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqDimensions) Lower(value MoqDimensions) C.RustBuffer {
	return LowerIntoRustBuffer[MoqDimensions](c, value)
}

func (c FfiConverterMoqDimensions) LowerExternal(value MoqDimensions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqDimensions](c, value))
}

func (c FfiConverterMoqDimensions) Write(writer io.Writer, value MoqDimensions) {
	FfiConverterUint32INSTANCE.Write(writer, value.Width)
	FfiConverterUint32INSTANCE.Write(writer, value.Height)
}

type FfiDestroyerMoqDimensions struct{}

func (_ FfiDestroyerMoqDimensions) Destroy(value MoqDimensions) {
	value.Destroy()
}

// Options for fetching one past group by sequence.
type MoqFetchGroupOptions struct {
	// Delivery priority for the fetch stream; higher values preempt lower ones.
	Priority uint8
}

func (r *MoqFetchGroupOptions) Destroy() {
	FfiDestroyerUint8{}.Destroy(r.Priority)
}

type FfiConverterMoqFetchGroupOptions struct{}

var FfiConverterMoqFetchGroupOptionsINSTANCE = FfiConverterMoqFetchGroupOptions{}

func (c FfiConverterMoqFetchGroupOptions) Lift(rb RustBufferI) MoqFetchGroupOptions {
	return LiftFromRustBuffer[MoqFetchGroupOptions](c, rb)
}

func (c FfiConverterMoqFetchGroupOptions) Read(reader io.Reader) MoqFetchGroupOptions {
	return MoqFetchGroupOptions{
		FfiConverterUint8INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqFetchGroupOptions) Lower(value MoqFetchGroupOptions) C.RustBuffer {
	return LowerIntoRustBuffer[MoqFetchGroupOptions](c, value)
}

func (c FfiConverterMoqFetchGroupOptions) LowerExternal(value MoqFetchGroupOptions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqFetchGroupOptions](c, value))
}

func (c FfiConverterMoqFetchGroupOptions) Write(writer io.Writer, value MoqFetchGroupOptions) {
	FfiConverterUint8INSTANCE.Write(writer, value.Priority)
}

type FfiDestroyerMoqFetchGroupOptions struct{}

func (_ FfiDestroyerMoqFetchGroupOptions) Destroy(value MoqFetchGroupOptions) {
	value.Destroy()
}

// A payload and the time it should be presented.
//
// The unit of both writing and raw reading: every producer write takes one of these, and a
// raw (non-media) read returns one. Media reads return a [`MoqMediaFrame`] instead, which
// adds the codec-derived keyframe flag.
type MoqFrame struct {
	// The frame payload.
	Payload []byte
	// Presentation timestamp in microseconds.
	TimestampUs uint64
}

func (r *MoqFrame) Destroy() {
	FfiDestroyerBytes{}.Destroy(r.Payload)
	FfiDestroyerUint64{}.Destroy(r.TimestampUs)
}

type FfiConverterMoqFrame struct{}

var FfiConverterMoqFrameINSTANCE = FfiConverterMoqFrame{}

func (c FfiConverterMoqFrame) Lift(rb RustBufferI) MoqFrame {
	return LiftFromRustBuffer[MoqFrame](c, rb)
}

func (c FfiConverterMoqFrame) Read(reader io.Reader) MoqFrame {
	return MoqFrame{
		FfiConverterBytesINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqFrame) Lower(value MoqFrame) C.RustBuffer {
	return LowerIntoRustBuffer[MoqFrame](c, value)
}

func (c FfiConverterMoqFrame) LowerExternal(value MoqFrame) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqFrame](c, value))
}

func (c FfiConverterMoqFrame) Write(writer io.Writer, value MoqFrame) {
	FfiConverterBytesINSTANCE.Write(writer, value.Payload)
	FfiConverterUint64INSTANCE.Write(writer, value.TimestampUs)
}

type FfiDestroyerMoqFrame struct{}

func (_ FfiDestroyerMoqFrame) Destroy(value MoqFrame) {
	value.Destroy()
}

// Options for a JSON snapshot track (lossy latest-value mode).
//
// The same config is passed to both the producer and the consumer, but the consumer reads only
// [`compression`](Self::compression); [`delta_ratio`](Self::delta_ratio) is producer-only.
type MoqJsonSnapshotConfig struct {
	// How aggressively the producer emits deltas instead of full snapshots. `0` disables deltas
	// (one snapshot per group); a positive value allows roughly that many snapshots' worth of
	// deltas before rolling a new group. Ignored by the consumer.
	DeltaRatio uint32
	// DEFLATE-compress each group. Must match on the producer and consumer.
	Compression bool
}

func (r *MoqJsonSnapshotConfig) Destroy() {
	FfiDestroyerUint32{}.Destroy(r.DeltaRatio)
	FfiDestroyerBool{}.Destroy(r.Compression)
}

type FfiConverterMoqJsonSnapshotConfig struct{}

var FfiConverterMoqJsonSnapshotConfigINSTANCE = FfiConverterMoqJsonSnapshotConfig{}

func (c FfiConverterMoqJsonSnapshotConfig) Lift(rb RustBufferI) MoqJsonSnapshotConfig {
	return LiftFromRustBuffer[MoqJsonSnapshotConfig](c, rb)
}

func (c FfiConverterMoqJsonSnapshotConfig) Read(reader io.Reader) MoqJsonSnapshotConfig {
	return MoqJsonSnapshotConfig{
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqJsonSnapshotConfig) Lower(value MoqJsonSnapshotConfig) C.RustBuffer {
	return LowerIntoRustBuffer[MoqJsonSnapshotConfig](c, value)
}

func (c FfiConverterMoqJsonSnapshotConfig) LowerExternal(value MoqJsonSnapshotConfig) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqJsonSnapshotConfig](c, value))
}

func (c FfiConverterMoqJsonSnapshotConfig) Write(writer io.Writer, value MoqJsonSnapshotConfig) {
	FfiConverterUint32INSTANCE.Write(writer, value.DeltaRatio)
	FfiConverterBoolINSTANCE.Write(writer, value.Compression)
}

type FfiDestroyerMoqJsonSnapshotConfig struct{}

func (_ FfiDestroyerMoqJsonSnapshotConfig) Destroy(value MoqJsonSnapshotConfig) {
	value.Destroy()
}

// Options for a JSON stream track (lossless append-log mode).
//
// The same config is passed to both the producer and the consumer.
type MoqJsonStreamConfig struct {
	// DEFLATE-compress the group. Must match on the producer and consumer.
	Compression bool
}

func (r *MoqJsonStreamConfig) Destroy() {
	FfiDestroyerBool{}.Destroy(r.Compression)
}

type FfiConverterMoqJsonStreamConfig struct{}

var FfiConverterMoqJsonStreamConfigINSTANCE = FfiConverterMoqJsonStreamConfig{}

func (c FfiConverterMoqJsonStreamConfig) Lift(rb RustBufferI) MoqJsonStreamConfig {
	return LiftFromRustBuffer[MoqJsonStreamConfig](c, rb)
}

func (c FfiConverterMoqJsonStreamConfig) Read(reader io.Reader) MoqJsonStreamConfig {
	return MoqJsonStreamConfig{
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqJsonStreamConfig) Lower(value MoqJsonStreamConfig) C.RustBuffer {
	return LowerIntoRustBuffer[MoqJsonStreamConfig](c, value)
}

func (c FfiConverterMoqJsonStreamConfig) LowerExternal(value MoqJsonStreamConfig) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqJsonStreamConfig](c, value))
}

func (c FfiConverterMoqJsonStreamConfig) Write(writer io.Writer, value MoqJsonStreamConfig) {
	FfiConverterBoolINSTANCE.Write(writer, value.Compression)
}

type FfiDestroyerMoqJsonStreamConfig struct{}

func (_ FfiDestroyerMoqJsonStreamConfig) Destroy(value MoqJsonStreamConfig) {
	value.Destroy()
}

// A [`MoqFrame`] plus the codec metadata a media track carries.
type MoqMediaFrame struct {
	// The frame payload.
	Payload []byte
	// Presentation timestamp in microseconds.
	TimestampUs uint64
	// Whether this frame opens a group or is a video keyframe; audio is true only at a group start.
	Keyframe bool
}

func (r *MoqMediaFrame) Destroy() {
	FfiDestroyerBytes{}.Destroy(r.Payload)
	FfiDestroyerUint64{}.Destroy(r.TimestampUs)
	FfiDestroyerBool{}.Destroy(r.Keyframe)
}

type FfiConverterMoqMediaFrame struct{}

var FfiConverterMoqMediaFrameINSTANCE = FfiConverterMoqMediaFrame{}

func (c FfiConverterMoqMediaFrame) Lift(rb RustBufferI) MoqMediaFrame {
	return LiftFromRustBuffer[MoqMediaFrame](c, rb)
}

func (c FfiConverterMoqMediaFrame) Read(reader io.Reader) MoqMediaFrame {
	return MoqMediaFrame{
		FfiConverterBytesINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqMediaFrame) Lower(value MoqMediaFrame) C.RustBuffer {
	return LowerIntoRustBuffer[MoqMediaFrame](c, value)
}

func (c FfiConverterMoqMediaFrame) LowerExternal(value MoqMediaFrame) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqMediaFrame](c, value))
}

func (c FfiConverterMoqMediaFrame) Write(writer io.Writer, value MoqMediaFrame) {
	FfiConverterBytesINSTANCE.Write(writer, value.Payload)
	FfiConverterUint64INSTANCE.Write(writer, value.TimestampUs)
	FfiConverterBoolINSTANCE.Write(writer, value.Keyframe)
}

type FfiDestroyerMoqMediaFrame struct{}

func (_ FfiDestroyerMoqMediaFrame) Destroy(value MoqMediaFrame) {
	value.Destroy()
}

// Config used when creating an origin.
type MoqOriginConfig struct {
	// Maximum cached group bytes across broadcasts under this origin. Null is unbounded.
	CacheCapacityBytes *uint64
}

func (r *MoqOriginConfig) Destroy() {
	FfiDestroyerOptionalUint64{}.Destroy(r.CacheCapacityBytes)
}

type FfiConverterMoqOriginConfig struct{}

var FfiConverterMoqOriginConfigINSTANCE = FfiConverterMoqOriginConfig{}

func (c FfiConverterMoqOriginConfig) Lift(rb RustBufferI) MoqOriginConfig {
	return LiftFromRustBuffer[MoqOriginConfig](c, rb)
}

func (c FfiConverterMoqOriginConfig) Read(reader io.Reader) MoqOriginConfig {
	return MoqOriginConfig{
		FfiConverterOptionalUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqOriginConfig) Lower(value MoqOriginConfig) C.RustBuffer {
	return LowerIntoRustBuffer[MoqOriginConfig](c, value)
}

func (c FfiConverterMoqOriginConfig) LowerExternal(value MoqOriginConfig) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqOriginConfig](c, value))
}

func (c FfiConverterMoqOriginConfig) Write(writer io.Writer, value MoqOriginConfig) {
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.CacheCapacityBytes)
}

type FfiDestroyerMoqOriginConfig struct{}

func (_ FfiDestroyerMoqOriginConfig) Destroy(value MoqOriginConfig) {
	value.Destroy()
}

// A protocol failure a peer sent (or this side will send): scope, verbatim code, kind,
// and a diagnostic message.
type MoqProtocolError struct {
	// Whether this code is from the session or stream registry.
	Scope MoqErrorScope
	// The integer on the wire, kept verbatim. Do not re-derive this from [`Self::kind`]:
	// App and Unknown each cover many codes, and the same kind is a different integer in each scope.
	Code uint32
	// The known kind when the code is recognized, otherwise [`MoqProtocolKind::App`] or
	// [`MoqProtocolKind::Unknown`].
	Kind MoqProtocolKind
	// Human-readable reason, for logs. Do not parse this.
	Message string
}

func (r *MoqProtocolError) Destroy() {
	FfiDestroyerMoqErrorScope{}.Destroy(r.Scope)
	FfiDestroyerUint32{}.Destroy(r.Code)
	FfiDestroyerMoqProtocolKind{}.Destroy(r.Kind)
	FfiDestroyerString{}.Destroy(r.Message)
}

type FfiConverterMoqProtocolError struct{}

var FfiConverterMoqProtocolErrorINSTANCE = FfiConverterMoqProtocolError{}

func (c FfiConverterMoqProtocolError) Lift(rb RustBufferI) MoqProtocolError {
	return LiftFromRustBuffer[MoqProtocolError](c, rb)
}

func (c FfiConverterMoqProtocolError) Read(reader io.Reader) MoqProtocolError {
	return MoqProtocolError{
		FfiConverterMoqErrorScopeINSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterMoqProtocolKindINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqProtocolError) Lower(value MoqProtocolError) C.RustBuffer {
	return LowerIntoRustBuffer[MoqProtocolError](c, value)
}

func (c FfiConverterMoqProtocolError) LowerExternal(value MoqProtocolError) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqProtocolError](c, value))
}

func (c FfiConverterMoqProtocolError) Write(writer io.Writer, value MoqProtocolError) {
	FfiConverterMoqErrorScopeINSTANCE.Write(writer, value.Scope)
	FfiConverterUint32INSTANCE.Write(writer, value.Code)
	FfiConverterMoqProtocolKindINSTANCE.Write(writer, value.Kind)
	FfiConverterStringINSTANCE.Write(writer, value.Message)
}

type FfiDestroyerMoqProtocolError struct{}

func (_ FfiDestroyerMoqProtocolError) Destroy(value MoqProtocolError) {
	value.Destroy()
}

// A path-prefix route: hops and costs for an advertisement.
//
// Pair one with `MoqBroadcastProducer::announce` for an exact path, or with
// `MoqOriginProducer::dynamic` for a prefix. Observe them with
// `MoqOriginConsumer::announced`. A route claims capability, not inventory: by
// convention a publisher announces each broadcast's exact path, so subscribers
// can enumerate broadcasts, while a service advertises a prefix and answers
// whatever is requested beneath it.
type MoqRoute struct {
	// Hop ids of the relay hops the route traversed, oldest first. 0 is the
	// anonymous mark and is legal on a received chain.
	Hops []uint64
	// Preference among routes covering the same prefix: lower wins. A publisher
	// sets its production cost here: zero for content it is already producing,
	// larger for content it would have to start producing on demand.
	Cost uint64
	// The same path with every warm discount removed: what pulling the content
	// would cost if no relay along it were carrying anything. `None` means the
	// same as `cost`, which is right for a publisher seeding a production cost.
	Cold *uint64
	// Whether the chain holds a 0 anywhere. An anonymous route ranks below every
	// fully identified one, whatever the costs say.
	Anonymous bool
}

func (r *MoqRoute) Destroy() {
	FfiDestroyerSequenceUint64{}.Destroy(r.Hops)
	FfiDestroyerUint64{}.Destroy(r.Cost)
	FfiDestroyerOptionalUint64{}.Destroy(r.Cold)
	FfiDestroyerBool{}.Destroy(r.Anonymous)
}

type FfiConverterMoqRoute struct{}

var FfiConverterMoqRouteINSTANCE = FfiConverterMoqRoute{}

func (c FfiConverterMoqRoute) Lift(rb RustBufferI) MoqRoute {
	return LiftFromRustBuffer[MoqRoute](c, rb)
}

func (c FfiConverterMoqRoute) Read(reader io.Reader) MoqRoute {
	return MoqRoute{
		FfiConverterSequenceUint64INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqRoute) Lower(value MoqRoute) C.RustBuffer {
	return LowerIntoRustBuffer[MoqRoute](c, value)
}

func (c FfiConverterMoqRoute) LowerExternal(value MoqRoute) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqRoute](c, value))
}

func (c FfiConverterMoqRoute) Write(writer io.Writer, value MoqRoute) {
	FfiConverterSequenceUint64INSTANCE.Write(writer, value.Hops)
	FfiConverterUint64INSTANCE.Write(writer, value.Cost)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.Cold)
	FfiConverterBoolINSTANCE.Write(writer, value.Anonymous)
}

type FfiDestroyerMoqRoute struct{}

func (_ FfiDestroyerMoqRoute) Destroy(value MoqRoute) {
	value.Destroy()
}

// Subscriber-side delivery preferences, mirroring [`moq_net::track::Subscription`].
//
// Construct with the fields you care about; the rest default to moq-net's defaults
// (priority 0, no staleness tolerance, full group range).
type MoqSubscription struct {
	// Delivery priority; higher values preempt lower ones under bandwidth contention.
	Priority uint8
	// Maximum age of a non-latest group before it is skipped, in microseconds.
	// `0` skips immediately; a larger value tolerates that much reordering.
	//
	// Enforced both by the publisher's cache (sent on the wire) and by any local
	// buffering, such as `subscribe_media`'s jitter buffer.
	MaxAgeUs uint64
	// The lowest group to deliver (a floor), or null for none. A floor is not a
	// request: `max_age_us` is what asks for data, and delivery starts at the oldest
	// group at or above the floor within that budget (the latest group at the default
	// budget of 0).
	GroupStart *uint64
	// First group not to deliver (exclusive), or null for no end. `Some(0)` is the
	// empty range.
	GroupEnd *uint64
}

func (r *MoqSubscription) Destroy() {
	FfiDestroyerUint8{}.Destroy(r.Priority)
	FfiDestroyerUint64{}.Destroy(r.MaxAgeUs)
	FfiDestroyerOptionalUint64{}.Destroy(r.GroupStart)
	FfiDestroyerOptionalUint64{}.Destroy(r.GroupEnd)
}

type FfiConverterMoqSubscription struct{}

var FfiConverterMoqSubscriptionINSTANCE = FfiConverterMoqSubscription{}

func (c FfiConverterMoqSubscription) Lift(rb RustBufferI) MoqSubscription {
	return LiftFromRustBuffer[MoqSubscription](c, rb)
}

func (c FfiConverterMoqSubscription) Read(reader io.Reader) MoqSubscription {
	return MoqSubscription{
		FfiConverterUint8INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqSubscription) Lower(value MoqSubscription) C.RustBuffer {
	return LowerIntoRustBuffer[MoqSubscription](c, value)
}

func (c FfiConverterMoqSubscription) LowerExternal(value MoqSubscription) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqSubscription](c, value))
}

func (c FfiConverterMoqSubscription) Write(writer io.Writer, value MoqSubscription) {
	FfiConverterUint8INSTANCE.Write(writer, value.Priority)
	FfiConverterUint64INSTANCE.Write(writer, value.MaxAgeUs)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.GroupStart)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.GroupEnd)
}

type FfiDestroyerMoqSubscription struct{}

func (_ FfiDestroyerMoqSubscription) Destroy(value MoqSubscription) {
	value.Destroy()
}

// Publisher-side track properties, mirroring [`moq_net::track::Info`].
//
// Construct with the fields you care about; the rest use raw-track defaults
// (priority 127, the publisher's default max age, microsecond timescale).
type MoqTrackInfo struct {
	// Priority, used only to break ties between subscriptions of equal subscriber priority.
	// Higher is more urgent; the default 127 is the midpoint.
	Priority uint8
	// Maximum age of a non-latest group before the publisher evicts it, in
	// microseconds. Null uses the default. This is the publisher-side half of
	// [`MoqSubscription::max_age_us`](crate::consumer::MoqSubscription::max_age_us).
	MaxAgeUs *uint64
	// Per-frame timescale in ticks per second. Null uses microseconds.
	Timescale *uint64
}

func (r *MoqTrackInfo) Destroy() {
	FfiDestroyerUint8{}.Destroy(r.Priority)
	FfiDestroyerOptionalUint64{}.Destroy(r.MaxAgeUs)
	FfiDestroyerOptionalUint64{}.Destroy(r.Timescale)
}

type FfiConverterMoqTrackInfo struct{}

var FfiConverterMoqTrackInfoINSTANCE = FfiConverterMoqTrackInfo{}

func (c FfiConverterMoqTrackInfo) Lift(rb RustBufferI) MoqTrackInfo {
	return LiftFromRustBuffer[MoqTrackInfo](c, rb)
}

func (c FfiConverterMoqTrackInfo) Read(reader io.Reader) MoqTrackInfo {
	return MoqTrackInfo{
		FfiConverterUint8INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqTrackInfo) Lower(value MoqTrackInfo) C.RustBuffer {
	return LowerIntoRustBuffer[MoqTrackInfo](c, value)
}

func (c FfiConverterMoqTrackInfo) LowerExternal(value MoqTrackInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqTrackInfo](c, value))
}

func (c FfiConverterMoqTrackInfo) Write(writer io.Writer, value MoqTrackInfo) {
	FfiConverterUint8INSTANCE.Write(writer, value.Priority)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.MaxAgeUs)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.Timescale)
}

type FfiDestroyerMoqTrackInfo struct{}

func (_ FfiDestroyerMoqTrackInfo) Destroy(value MoqTrackInfo) {
	value.Destroy()
}

type MoqVideo struct {
	// Human-readable rendition name for track pickers.
	Label *string
	// The broadcast serving this rendition's track, relative to the catalog's own broadcast
	// (e.g. `./source`). Absent or empty means the catalog's broadcast.
	Broadcast     *string
	Codec         string
	Description   *[]byte
	Coded         *MoqDimensions
	DisplayAspect *MoqDimensions
	Bitrate       *uint64
	// Whether the publisher recommends temporarily avoiding this rendition.
	Stalled   bool
	Framerate *float64
	Container MoqContainer
}

func (r *MoqVideo) Destroy() {
	FfiDestroyerOptionalString{}.Destroy(r.Label)
	FfiDestroyerOptionalString{}.Destroy(r.Broadcast)
	FfiDestroyerString{}.Destroy(r.Codec)
	FfiDestroyerOptionalBytes{}.Destroy(r.Description)
	FfiDestroyerOptionalMoqDimensions{}.Destroy(r.Coded)
	FfiDestroyerOptionalMoqDimensions{}.Destroy(r.DisplayAspect)
	FfiDestroyerOptionalUint64{}.Destroy(r.Bitrate)
	FfiDestroyerBool{}.Destroy(r.Stalled)
	FfiDestroyerOptionalFloat64{}.Destroy(r.Framerate)
	FfiDestroyerMoqContainer{}.Destroy(r.Container)
}

type FfiConverterMoqVideo struct{}

var FfiConverterMoqVideoINSTANCE = FfiConverterMoqVideo{}

func (c FfiConverterMoqVideo) Lift(rb RustBufferI) MoqVideo {
	return LiftFromRustBuffer[MoqVideo](c, rb)
}

func (c FfiConverterMoqVideo) Read(reader io.Reader) MoqVideo {
	return MoqVideo{
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalBytesINSTANCE.Read(reader),
		FfiConverterOptionalMoqDimensionsINSTANCE.Read(reader),
		FfiConverterOptionalMoqDimensionsINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalFloat64INSTANCE.Read(reader),
		FfiConverterMoqContainerINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqVideo) Lower(value MoqVideo) C.RustBuffer {
	return LowerIntoRustBuffer[MoqVideo](c, value)
}

func (c FfiConverterMoqVideo) LowerExternal(value MoqVideo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqVideo](c, value))
}

func (c FfiConverterMoqVideo) Write(writer io.Writer, value MoqVideo) {
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Label)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Broadcast)
	FfiConverterStringINSTANCE.Write(writer, value.Codec)
	FfiConverterOptionalBytesINSTANCE.Write(writer, value.Description)
	FfiConverterOptionalMoqDimensionsINSTANCE.Write(writer, value.Coded)
	FfiConverterOptionalMoqDimensionsINSTANCE.Write(writer, value.DisplayAspect)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.Bitrate)
	FfiConverterBoolINSTANCE.Write(writer, value.Stalled)
	FfiConverterOptionalFloat64INSTANCE.Write(writer, value.Framerate)
	FfiConverterMoqContainerINSTANCE.Write(writer, value.Container)
}

type FfiDestroyerMoqVideo struct{}

func (_ FfiDestroyerMoqVideo) Destroy(value MoqVideo) {
	value.Destroy()
}

// One decoded video frame: packed pixels plus the layout and size they
// actually decoded to.
//
// Unlike [`MoqVideoFrame`] on the publish side, this carries dimensions: there
// they are fixed by the encoder config, here they are whatever the stream
// turned out to be, and `resize` is only best effort.
type MoqVideoDecodedFrame struct {
	// Presentation timestamp, in microseconds.
	TimestampUs uint64
	// Frame width in pixels.
	Width uint32
	// Frame height in pixels.
	Height uint32
	// The pixels, in `format`: I420 is Y, then U, then V (`width * height * 3 /
	// 2` bytes); RGBA is `width * height * 4` bytes. Neither has row padding.
	Data []byte
	// The layout `data` is in, which is what
	// [`MoqVideoDecoderOutput::format`] asked for.
	Format MoqVideoPixelFormat
}

func (r *MoqVideoDecodedFrame) Destroy() {
	FfiDestroyerUint64{}.Destroy(r.TimestampUs)
	FfiDestroyerUint32{}.Destroy(r.Width)
	FfiDestroyerUint32{}.Destroy(r.Height)
	FfiDestroyerBytes{}.Destroy(r.Data)
	FfiDestroyerMoqVideoPixelFormat{}.Destroy(r.Format)
}

type FfiConverterMoqVideoDecodedFrame struct{}

var FfiConverterMoqVideoDecodedFrameINSTANCE = FfiConverterMoqVideoDecodedFrame{}

func (c FfiConverterMoqVideoDecodedFrame) Lift(rb RustBufferI) MoqVideoDecodedFrame {
	return LiftFromRustBuffer[MoqVideoDecodedFrame](c, rb)
}

func (c FfiConverterMoqVideoDecodedFrame) Read(reader io.Reader) MoqVideoDecodedFrame {
	return MoqVideoDecodedFrame{
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterBytesINSTANCE.Read(reader),
		FfiConverterMoqVideoPixelFormatINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqVideoDecodedFrame) Lower(value MoqVideoDecodedFrame) C.RustBuffer {
	return LowerIntoRustBuffer[MoqVideoDecodedFrame](c, value)
}

func (c FfiConverterMoqVideoDecodedFrame) LowerExternal(value MoqVideoDecodedFrame) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqVideoDecodedFrame](c, value))
}

func (c FfiConverterMoqVideoDecodedFrame) Write(writer io.Writer, value MoqVideoDecodedFrame) {
	FfiConverterUint64INSTANCE.Write(writer, value.TimestampUs)
	FfiConverterUint32INSTANCE.Write(writer, value.Width)
	FfiConverterUint32INSTANCE.Write(writer, value.Height)
	FfiConverterBytesINSTANCE.Write(writer, value.Data)
	FfiConverterMoqVideoPixelFormatINSTANCE.Write(writer, value.Format)
}

type FfiDestroyerMoqVideoDecodedFrame struct{}

func (_ FfiDestroyerMoqVideoDecodedFrame) Destroy(value MoqVideoDecodedFrame) {
	value.Destroy()
}

// How a subscriber wants decoded video delivered.
//
// A decoder's native output is flattened to CPU pixels at delivery, since the
// FFI boundary can't hand back a GPU surface; `format` picks the layout it is
// flattened to.
type MoqVideoDecoderOutput struct {
	// Ask the decoder to emit frames at this size instead of the stream's
	// native one. Best effort: only NVDEC has a built-in scaler and honors it for
	// free; VideoToolbox, Media Foundation, MediaCodec, VAAPI, V4L2, and openh264
	// ignore it and decode at the stream's native size. Read each frame's own
	// dimensions rather than assuming this took. Both dimensions must be even.
	Resize *MoqDimensions
	// Upper bound on buffering before skipping a stalled group, in
	// microseconds. Same knob as
	// [`MoqAudioDecoderOutput::max_age_us`](crate::audio::MoqAudioDecoderOutput::max_age_us).
	// `None` keeps the moq-mux default of zero (skip aggressively).
	MaxAgeUs *uint64
	// CPU pixel layout every frame is delivered in. `None` delivers
	// [`MoqVideoPixelFormat::I420`], which is what a decoder produces natively,
	// so asking for RGBA costs a conversion per frame.
	//
	// Spelled as an option rather than an I420-valued field because uniffi has no
	// enum default, and a required field would break every existing caller.
	Format *MoqVideoPixelFormat
}

func (r *MoqVideoDecoderOutput) Destroy() {
	FfiDestroyerOptionalMoqDimensions{}.Destroy(r.Resize)
	FfiDestroyerOptionalUint64{}.Destroy(r.MaxAgeUs)
	FfiDestroyerOptionalMoqVideoPixelFormat{}.Destroy(r.Format)
}

type FfiConverterMoqVideoDecoderOutput struct{}

var FfiConverterMoqVideoDecoderOutputINSTANCE = FfiConverterMoqVideoDecoderOutput{}

func (c FfiConverterMoqVideoDecoderOutput) Lift(rb RustBufferI) MoqVideoDecoderOutput {
	return LiftFromRustBuffer[MoqVideoDecoderOutput](c, rb)
}

func (c FfiConverterMoqVideoDecoderOutput) Read(reader io.Reader) MoqVideoDecoderOutput {
	return MoqVideoDecoderOutput{
		FfiConverterOptionalMoqDimensionsINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalMoqVideoPixelFormatINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqVideoDecoderOutput) Lower(value MoqVideoDecoderOutput) C.RustBuffer {
	return LowerIntoRustBuffer[MoqVideoDecoderOutput](c, value)
}

func (c FfiConverterMoqVideoDecoderOutput) LowerExternal(value MoqVideoDecoderOutput) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqVideoDecoderOutput](c, value))
}

func (c FfiConverterMoqVideoDecoderOutput) Write(writer io.Writer, value MoqVideoDecoderOutput) {
	FfiConverterOptionalMoqDimensionsINSTANCE.Write(writer, value.Resize)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.MaxAgeUs)
	FfiConverterOptionalMoqVideoPixelFormatINSTANCE.Write(writer, value.Format)
}

type FfiDestroyerMoqVideoDecoderOutput struct{}

func (_ FfiDestroyerMoqVideoDecoderOutput) Destroy(value MoqVideoDecoderOutput) {
	value.Destroy()
}

// Raw frame layout the caller will pass to [`MoqVideoProducer::write`], plus
// the resolution and rate the encoder is opened at. Every written frame must
// match `width` x `height`; scale before writing if your source moves.
type MoqVideoEncoderInput struct {
	Format MoqVideoPixelFormat
	// Encoded width in pixels. Must be even (I420 chroma is subsampled 2x2).
	Width uint32
	// Encoded height in pixels. Must be even.
	Height uint32
	// Nominal frames per second, used for the codec time base and the default
	// bitrate and keyframe interval. Must be non-zero.
	Framerate uint32
}

func (r *MoqVideoEncoderInput) Destroy() {
	FfiDestroyerMoqVideoPixelFormat{}.Destroy(r.Format)
	FfiDestroyerUint32{}.Destroy(r.Width)
	FfiDestroyerUint32{}.Destroy(r.Height)
	FfiDestroyerUint32{}.Destroy(r.Framerate)
}

type FfiConverterMoqVideoEncoderInput struct{}

var FfiConverterMoqVideoEncoderInputINSTANCE = FfiConverterMoqVideoEncoderInput{}

func (c FfiConverterMoqVideoEncoderInput) Lift(rb RustBufferI) MoqVideoEncoderInput {
	return LiftFromRustBuffer[MoqVideoEncoderInput](c, rb)
}

func (c FfiConverterMoqVideoEncoderInput) Read(reader io.Reader) MoqVideoEncoderInput {
	return MoqVideoEncoderInput{
		FfiConverterMoqVideoPixelFormatINSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqVideoEncoderInput) Lower(value MoqVideoEncoderInput) C.RustBuffer {
	return LowerIntoRustBuffer[MoqVideoEncoderInput](c, value)
}

func (c FfiConverterMoqVideoEncoderInput) LowerExternal(value MoqVideoEncoderInput) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqVideoEncoderInput](c, value))
}

func (c FfiConverterMoqVideoEncoderInput) Write(writer io.Writer, value MoqVideoEncoderInput) {
	FfiConverterMoqVideoPixelFormatINSTANCE.Write(writer, value.Format)
	FfiConverterUint32INSTANCE.Write(writer, value.Width)
	FfiConverterUint32INSTANCE.Write(writer, value.Height)
	FfiConverterUint32INSTANCE.Write(writer, value.Framerate)
}

type FfiDestroyerMoqVideoEncoderInput struct{}

func (_ FfiDestroyerMoqVideoEncoderInput) Destroy(value MoqVideoEncoderInput) {
	value.Destroy()
}

// Codec-side configuration.
type MoqVideoEncoderOutput struct {
	Codec MoqVideoCodec
	// Track name. `None` derives a unique name from the codec.
	Track *string
	// Target bitrate in bits per second. `None` derives one from the resolution
	// and framerate.
	Bitrate *uint64
	// Keyframe interval in frames: a subscriber joining mid-stream waits at most
	// this many frames before it can decode. `None` uses ~2 seconds.
	Gop *uint32
	// Encoder implementation preference. Pass
	// [`MoqVideoEncoderKind::Auto`] unless you need a specific backend.
	Kind MoqVideoEncoderKind
}

func (r *MoqVideoEncoderOutput) Destroy() {
	FfiDestroyerMoqVideoCodec{}.Destroy(r.Codec)
	FfiDestroyerOptionalString{}.Destroy(r.Track)
	FfiDestroyerOptionalUint64{}.Destroy(r.Bitrate)
	FfiDestroyerOptionalUint32{}.Destroy(r.Gop)
	FfiDestroyerMoqVideoEncoderKind{}.Destroy(r.Kind)
}

type FfiConverterMoqVideoEncoderOutput struct{}

var FfiConverterMoqVideoEncoderOutputINSTANCE = FfiConverterMoqVideoEncoderOutput{}

func (c FfiConverterMoqVideoEncoderOutput) Lift(rb RustBufferI) MoqVideoEncoderOutput {
	return LiftFromRustBuffer[MoqVideoEncoderOutput](c, rb)
}

func (c FfiConverterMoqVideoEncoderOutput) Read(reader io.Reader) MoqVideoEncoderOutput {
	return MoqVideoEncoderOutput{
		FfiConverterMoqVideoCodecINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterMoqVideoEncoderKindINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqVideoEncoderOutput) Lower(value MoqVideoEncoderOutput) C.RustBuffer {
	return LowerIntoRustBuffer[MoqVideoEncoderOutput](c, value)
}

func (c FfiConverterMoqVideoEncoderOutput) LowerExternal(value MoqVideoEncoderOutput) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqVideoEncoderOutput](c, value))
}

func (c FfiConverterMoqVideoEncoderOutput) Write(writer io.Writer, value MoqVideoEncoderOutput) {
	FfiConverterMoqVideoCodecINSTANCE.Write(writer, value.Codec)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Track)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.Bitrate)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.Gop)
	FfiConverterMoqVideoEncoderKindINSTANCE.Write(writer, value.Kind)
}

type FfiDestroyerMoqVideoEncoderOutput struct{}

func (_ FfiDestroyerMoqVideoEncoderOutput) Destroy(value MoqVideoEncoderOutput) {
	value.Destroy()
}

// One raw video frame: pixels plus a presentation timestamp.
//
// The pixel format and resolution are fixed by [`MoqVideoEncoderInput`] at
// publish time, so a frame carries neither. `data` is exactly one picture in
// that layout.
type MoqVideoFrame struct {
	// Presentation timestamp, in microseconds.
	TimestampUs uint64
	// The pixels, in the configured layout.
	Data []byte
}

func (r *MoqVideoFrame) Destroy() {
	FfiDestroyerUint64{}.Destroy(r.TimestampUs)
	FfiDestroyerBytes{}.Destroy(r.Data)
}

type FfiConverterMoqVideoFrame struct{}

var FfiConverterMoqVideoFrameINSTANCE = FfiConverterMoqVideoFrame{}

func (c FfiConverterMoqVideoFrame) Lift(rb RustBufferI) MoqVideoFrame {
	return LiftFromRustBuffer[MoqVideoFrame](c, rb)
}

func (c FfiConverterMoqVideoFrame) Read(reader io.Reader) MoqVideoFrame {
	return MoqVideoFrame{
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterBytesINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqVideoFrame) Lower(value MoqVideoFrame) C.RustBuffer {
	return LowerIntoRustBuffer[MoqVideoFrame](c, value)
}

func (c FfiConverterMoqVideoFrame) LowerExternal(value MoqVideoFrame) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqVideoFrame](c, value))
}

func (c FfiConverterMoqVideoFrame) Write(writer io.Writer, value MoqVideoFrame) {
	FfiConverterUint64INSTANCE.Write(writer, value.TimestampUs)
	FfiConverterBytesINSTANCE.Write(writer, value.Data)
}

type FfiDestroyerMoqVideoFrame struct{}

func (_ FfiDestroyerMoqVideoFrame) Destroy(value MoqVideoFrame) {
	value.Destroy()
}

// Caller-provided video catalog fields for [`MoqVideoInit`].
//
// Every field is optional and fills only a gap the stream leaves; a value the stream detects wins.
// Publishing the catalog before the first keyframe needs at least the codec, which comes from the
// [`MoqVideoInit`] format. Audio has no equivalent: it resolves entirely from its init bytes.
type MoqVideoHint struct {
	// The encoded pixel dimensions.
	Coded *MoqDimensions
	// The display aspect ratio.
	DisplayAspect *MoqDimensions
	// The maximum bitrate in bits per second.
	Bitrate *uint64
	// The frame rate in frames per second.
	Framerate *float64
	// Whether the decoder should optimize for latency.
	OptimizeForLatency *bool
}

func (r *MoqVideoHint) Destroy() {
	FfiDestroyerOptionalMoqDimensions{}.Destroy(r.Coded)
	FfiDestroyerOptionalMoqDimensions{}.Destroy(r.DisplayAspect)
	FfiDestroyerOptionalUint64{}.Destroy(r.Bitrate)
	FfiDestroyerOptionalFloat64{}.Destroy(r.Framerate)
	FfiDestroyerOptionalBool{}.Destroy(r.OptimizeForLatency)
}

type FfiConverterMoqVideoHint struct{}

var FfiConverterMoqVideoHintINSTANCE = FfiConverterMoqVideoHint{}

func (c FfiConverterMoqVideoHint) Lift(rb RustBufferI) MoqVideoHint {
	return LiftFromRustBuffer[MoqVideoHint](c, rb)
}

func (c FfiConverterMoqVideoHint) Read(reader io.Reader) MoqVideoHint {
	return MoqVideoHint{
		FfiConverterOptionalMoqDimensionsINSTANCE.Read(reader),
		FfiConverterOptionalMoqDimensionsINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalFloat64INSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqVideoHint) Lower(value MoqVideoHint) C.RustBuffer {
	return LowerIntoRustBuffer[MoqVideoHint](c, value)
}

func (c FfiConverterMoqVideoHint) LowerExternal(value MoqVideoHint) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqVideoHint](c, value))
}

func (c FfiConverterMoqVideoHint) Write(writer io.Writer, value MoqVideoHint) {
	FfiConverterOptionalMoqDimensionsINSTANCE.Write(writer, value.Coded)
	FfiConverterOptionalMoqDimensionsINSTANCE.Write(writer, value.DisplayAspect)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.Bitrate)
	FfiConverterOptionalFloat64INSTANCE.Write(writer, value.Framerate)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.OptimizeForLatency)
}

type FfiDestroyerMoqVideoHint struct{}

func (_ FfiDestroyerMoqVideoHint) Destroy(value MoqVideoHint) {
	value.Destroy()
}

// What a video publish needs: a format, optional init bytes, a label, and hints.
//
// `data` may be empty for a format that resolves in band. A [`hint`](Self::hint) pins catalog
// fields the stream never reveals (bitrate) or publishes the catalog before the first keyframe.
type MoqVideoInit struct {
	// The video codec.
	Format MoqVideoFormat
	// Codec init bytes (an avcC, an hvcC, ...). May be empty for a format that resolves in band.
	Data []byte
	// Human-readable rendition name for a track picker.
	Label *string
	// Catalog fields the stream cannot reveal itself.
	Hint *MoqVideoHint
	// Track name. `None` derives a unique name from the format. Refused on a requested track,
	// which already carries its name.
	Track *string
}

func (r *MoqVideoInit) Destroy() {
	FfiDestroyerMoqVideoFormat{}.Destroy(r.Format)
	FfiDestroyerBytes{}.Destroy(r.Data)
	FfiDestroyerOptionalString{}.Destroy(r.Label)
	FfiDestroyerOptionalMoqVideoHint{}.Destroy(r.Hint)
	FfiDestroyerOptionalString{}.Destroy(r.Track)
}

type FfiConverterMoqVideoInit struct{}

var FfiConverterMoqVideoInitINSTANCE = FfiConverterMoqVideoInit{}

func (c FfiConverterMoqVideoInit) Lift(rb RustBufferI) MoqVideoInit {
	return LiftFromRustBuffer[MoqVideoInit](c, rb)
}

func (c FfiConverterMoqVideoInit) Read(reader io.Reader) MoqVideoInit {
	return MoqVideoInit{
		FfiConverterMoqVideoFormatINSTANCE.Read(reader),
		FfiConverterBytesINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalMoqVideoHintINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqVideoInit) Lower(value MoqVideoInit) C.RustBuffer {
	return LowerIntoRustBuffer[MoqVideoInit](c, value)
}

func (c FfiConverterMoqVideoInit) LowerExternal(value MoqVideoInit) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqVideoInit](c, value))
}

func (c FfiConverterMoqVideoInit) Write(writer io.Writer, value MoqVideoInit) {
	FfiConverterMoqVideoFormatINSTANCE.Write(writer, value.Format)
	FfiConverterBytesINSTANCE.Write(writer, value.Data)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Label)
	FfiConverterOptionalMoqVideoHintINSTANCE.Write(writer, value.Hint)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Track)
}

type FfiDestroyerMoqVideoInit struct{}

func (_ FfiDestroyerMoqVideoInit) Destroy(value MoqVideoInit) {
	value.Destroy()
}

// Catalog properties shared by every video rendition.
//
// Passing an absent field clears it from the next catalog snapshot rather than preserving the previous value.
type MoqVideoProperties struct {
	// Final rendered size after rotation, or absent to clear the explicit display size.
	Display *MoqDimensions
	// Clockwise rotation in degrees, or absent to clear the explicit rotation.
	Rotation *float64
	// Whether to flip horizontally after rotation, or absent to clear the explicit value.
	Flip *bool
}

func (r *MoqVideoProperties) Destroy() {
	FfiDestroyerOptionalMoqDimensions{}.Destroy(r.Display)
	FfiDestroyerOptionalFloat64{}.Destroy(r.Rotation)
	FfiDestroyerOptionalBool{}.Destroy(r.Flip)
}

type FfiConverterMoqVideoProperties struct{}

var FfiConverterMoqVideoPropertiesINSTANCE = FfiConverterMoqVideoProperties{}

func (c FfiConverterMoqVideoProperties) Lift(rb RustBufferI) MoqVideoProperties {
	return LiftFromRustBuffer[MoqVideoProperties](c, rb)
}

func (c FfiConverterMoqVideoProperties) Read(reader io.Reader) MoqVideoProperties {
	return MoqVideoProperties{
		FfiConverterOptionalMoqDimensionsINSTANCE.Read(reader),
		FfiConverterOptionalFloat64INSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqVideoProperties) Lower(value MoqVideoProperties) C.RustBuffer {
	return LowerIntoRustBuffer[MoqVideoProperties](c, value)
}

func (c FfiConverterMoqVideoProperties) LowerExternal(value MoqVideoProperties) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqVideoProperties](c, value))
}

func (c FfiConverterMoqVideoProperties) Write(writer io.Writer, value MoqVideoProperties) {
	FfiConverterOptionalMoqDimensionsINSTANCE.Write(writer, value.Display)
	FfiConverterOptionalFloat64INSTANCE.Write(writer, value.Rotation)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.Flip)
}

type FfiDestroyerMoqVideoProperties struct{}

func (_ FfiDestroyerMoqVideoProperties) Destroy(value MoqVideoProperties) {
	value.Destroy()
}

// A single audio codec an importer can parse.
type MoqAudioFormat uint

const (
	// Advanced Audio Coding, configured by an AudioSpecificConfig.
	MoqAudioFormatAac MoqAudioFormat = 1
	// Opus, configured by an OpusHead.
	MoqAudioFormatOpus MoqAudioFormat = 2
	// FLAC, configured by the `fLaC` marker plus its STREAMINFO block.
	MoqAudioFormatFlac MoqAudioFormat = 3
	// MPEG-1/2 Audio Layer III.
	MoqAudioFormatMp3 MoqAudioFormat = 4
)

type FfiConverterMoqAudioFormat struct{}

var FfiConverterMoqAudioFormatINSTANCE = FfiConverterMoqAudioFormat{}

func (c FfiConverterMoqAudioFormat) Lift(rb RustBufferI) MoqAudioFormat {
	return LiftFromRustBuffer[MoqAudioFormat](c, rb)
}

func (c FfiConverterMoqAudioFormat) Lower(value MoqAudioFormat) C.RustBuffer {
	return LowerIntoRustBuffer[MoqAudioFormat](c, value)
}

func (c FfiConverterMoqAudioFormat) LowerExternal(value MoqAudioFormat) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqAudioFormat](c, value))
}
func (FfiConverterMoqAudioFormat) Read(reader io.Reader) MoqAudioFormat {
	id := readInt32(reader)
	return MoqAudioFormat(id)
}

func (FfiConverterMoqAudioFormat) Write(writer io.Writer, value MoqAudioFormat) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerMoqAudioFormat struct{}

func (_ FfiDestroyerMoqAudioFormat) Destroy(value MoqAudioFormat) {
}

// Raw PCM sample format, mirroring WebCodecs `AudioData.format`.
//
// <https://developer.mozilla.org/en-US/docs/Web/API/AudioData/format>
type MoqAudioSampleFormat uint

const (
	MoqAudioSampleFormatU8        MoqAudioSampleFormat = 1
	MoqAudioSampleFormatS16       MoqAudioSampleFormat = 2
	MoqAudioSampleFormatS32       MoqAudioSampleFormat = 3
	MoqAudioSampleFormatF32       MoqAudioSampleFormat = 4
	MoqAudioSampleFormatU8Planar  MoqAudioSampleFormat = 5
	MoqAudioSampleFormatS16Planar MoqAudioSampleFormat = 6
	MoqAudioSampleFormatS32Planar MoqAudioSampleFormat = 7
	MoqAudioSampleFormatF32Planar MoqAudioSampleFormat = 8
)

type FfiConverterMoqAudioSampleFormat struct{}

var FfiConverterMoqAudioSampleFormatINSTANCE = FfiConverterMoqAudioSampleFormat{}

func (c FfiConverterMoqAudioSampleFormat) Lift(rb RustBufferI) MoqAudioSampleFormat {
	return LiftFromRustBuffer[MoqAudioSampleFormat](c, rb)
}

func (c FfiConverterMoqAudioSampleFormat) Lower(value MoqAudioSampleFormat) C.RustBuffer {
	return LowerIntoRustBuffer[MoqAudioSampleFormat](c, value)
}

func (c FfiConverterMoqAudioSampleFormat) LowerExternal(value MoqAudioSampleFormat) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqAudioSampleFormat](c, value))
}
func (FfiConverterMoqAudioSampleFormat) Read(reader io.Reader) MoqAudioSampleFormat {
	id := readInt32(reader)
	return MoqAudioSampleFormat(id)
}

func (FfiConverterMoqAudioSampleFormat) Write(writer io.Writer, value MoqAudioSampleFormat) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerMoqAudioSampleFormat struct{}

func (_ FfiDestroyerMoqAudioSampleFormat) Destroy(value MoqAudioSampleFormat) {
}

// A connection lifecycle transition reported by [`MoqSession::status`].
type MoqConnectionStatus uint

const (
	// A session connected (the first connect, or a reconnect after a drop).
	MoqConnectionStatusConnected MoqConnectionStatus = 1
	// The session dropped; a reconnect attempt follows.
	MoqConnectionStatusDisconnected MoqConnectionStatus = 2
	// The peer sent a GOAWAY; the replacement is being dialed while the old
	// session keeps serving.
	MoqConnectionStatusMigrating MoqConnectionStatus = 3
)

type FfiConverterMoqConnectionStatus struct{}

var FfiConverterMoqConnectionStatusINSTANCE = FfiConverterMoqConnectionStatus{}

func (c FfiConverterMoqConnectionStatus) Lift(rb RustBufferI) MoqConnectionStatus {
	return LiftFromRustBuffer[MoqConnectionStatus](c, rb)
}

func (c FfiConverterMoqConnectionStatus) Lower(value MoqConnectionStatus) C.RustBuffer {
	return LowerIntoRustBuffer[MoqConnectionStatus](c, value)
}

func (c FfiConverterMoqConnectionStatus) LowerExternal(value MoqConnectionStatus) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqConnectionStatus](c, value))
}
func (FfiConverterMoqConnectionStatus) Read(reader io.Reader) MoqConnectionStatus {
	id := readInt32(reader)
	return MoqConnectionStatus(id)
}

func (FfiConverterMoqConnectionStatus) Write(writer io.Writer, value MoqConnectionStatus) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerMoqConnectionStatus struct{}

func (_ FfiDestroyerMoqConnectionStatus) Destroy(value MoqConnectionStatus) {
}

// How a track's frames are packaged, as advertised in the catalog.
type MoqContainer interface {
	Destroy()
}

// The legacy hang container.
type MoqContainerLegacy struct {
}

func (e MoqContainerLegacy) Destroy() {
}

// CMAF (fMP4), carrying the initialization segment.
type MoqContainerCmaf struct {
	Init []byte
}

func (e MoqContainerCmaf) Destroy() {
	FfiDestroyerBytes{}.Destroy(e.Init)
}

// LOC, the low-overhead container.
type MoqContainerLoc struct {
}

func (e MoqContainerLoc) Destroy() {
}

type FfiConverterMoqContainer struct{}

var FfiConverterMoqContainerINSTANCE = FfiConverterMoqContainer{}

func (c FfiConverterMoqContainer) Lift(rb RustBufferI) MoqContainer {
	return LiftFromRustBuffer[MoqContainer](c, rb)
}

func (c FfiConverterMoqContainer) Lower(value MoqContainer) C.RustBuffer {
	return LowerIntoRustBuffer[MoqContainer](c, value)
}

func (c FfiConverterMoqContainer) LowerExternal(value MoqContainer) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqContainer](c, value))
}
func (FfiConverterMoqContainer) Read(reader io.Reader) MoqContainer {
	id := readInt32(reader)
	switch id {
	case 1:
		return MoqContainerLegacy{}
	case 2:
		return MoqContainerCmaf{
			FfiConverterBytesINSTANCE.Read(reader),
		}
	case 3:
		return MoqContainerLoc{}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterMoqContainer.Read()", id))
	}
}

func (FfiConverterMoqContainer) Write(writer io.Writer, value MoqContainer) {
	switch variant_value := value.(type) {
	case MoqContainerLegacy:
		writeInt32(writer, 1)
	case MoqContainerCmaf:
		writeInt32(writer, 2)
		FfiConverterBytesINSTANCE.Write(writer, variant_value.Init)
	case MoqContainerLoc:
		writeInt32(writer, 3)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterMoqContainer.Write", value))
	}
}

type FfiDestroyerMoqContainer struct{}

func (_ FfiDestroyerMoqContainer) Destroy(value MoqContainer) {
	value.Destroy()
}

// A container that publishes its own tracks, which may be more than one.
type MoqContainerFormat uint

const (
	// Fragmented MP4 / CMAF.
	MoqContainerFormatFmp4 MoqContainerFormat = 1
	// Matroska / WebM.
	MoqContainerFormatMkv MoqContainerFormat = 2
	// MPEG-2 transport stream.
	MoqContainerFormatTs MoqContainerFormat = 3
	// Flash Video, as used by RTMP.
	MoqContainerFormatFlv MoqContainerFormat = 4
)

type FfiConverterMoqContainerFormat struct{}

var FfiConverterMoqContainerFormatINSTANCE = FfiConverterMoqContainerFormat{}

func (c FfiConverterMoqContainerFormat) Lift(rb RustBufferI) MoqContainerFormat {
	return LiftFromRustBuffer[MoqContainerFormat](c, rb)
}

func (c FfiConverterMoqContainerFormat) Lower(value MoqContainerFormat) C.RustBuffer {
	return LowerIntoRustBuffer[MoqContainerFormat](c, value)
}

func (c FfiConverterMoqContainerFormat) LowerExternal(value MoqContainerFormat) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqContainerFormat](c, value))
}
func (FfiConverterMoqContainerFormat) Read(reader io.Reader) MoqContainerFormat {
	id := readInt32(reader)
	return MoqContainerFormat(id)
}

func (FfiConverterMoqContainerFormat) Write(writer io.Writer, value MoqContainerFormat) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerMoqContainerFormat struct{}

func (_ FfiDestroyerMoqContainerFormat) Destroy(value MoqContainerFormat) {
}

// Error returned by all UniFFI-exported functions.
//
// Exports `Display`, which the bindings surface as the error's string form.
type MoqError struct {
	err error
}

// Convenience method to turn *MoqError into error
// Avoiding treating nil pointer as non nil error interface
func (err *MoqError) AsError() error {
	if err == nil {
		return nil
	} else {
		return err
	}
}
func (_self *MoqError) String() string {
	_selfBuf := FfiConverterMoqErrorINSTANCE.Lower(_self)
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqerror_uniffi_trait_display(
				_selfBuf, _uniffiStatus),
		}
	}))
}

func (err MoqError) Error() string {
	return (&err).String()
}

func (err MoqError) Unwrap() error {
	return err.err
}

// Err* are used for checking error type with `errors.Is`
var ErrMoqErrorProtocol = fmt.Errorf("MoqErrorProtocol")
var ErrMoqErrorTransport = fmt.Errorf("MoqErrorTransport")
var ErrMoqErrorInternal = fmt.Errorf("MoqErrorInternal")
var ErrMoqErrorMedia = fmt.Errorf("MoqErrorMedia")
var ErrMoqErrorMux = fmt.Errorf("MoqErrorMux")
var ErrMoqErrorJsonTrack = fmt.Errorf("MoqErrorJsonTrack")
var ErrMoqErrorAudio = fmt.Errorf("MoqErrorAudio")
var ErrMoqErrorVideo = fmt.Errorf("MoqErrorVideo")
var ErrMoqErrorUrl = fmt.Errorf("MoqErrorUrl")
var ErrMoqErrorTimeOverflow = fmt.Errorf("MoqErrorTimeOverflow")
var ErrMoqErrorLogLevel = fmt.Errorf("MoqErrorLogLevel")
var ErrMoqErrorTask = fmt.Errorf("MoqErrorTask")
var ErrMoqErrorJson = fmt.Errorf("MoqErrorJson")
var ErrMoqErrorCancelled = fmt.Errorf("MoqErrorCancelled")
var ErrMoqErrorClosed = fmt.Errorf("MoqErrorClosed")
var ErrMoqErrorBusy = fmt.Errorf("MoqErrorBusy")
var ErrMoqErrorConnect = fmt.Errorf("MoqErrorConnect")
var ErrMoqErrorBind = fmt.Errorf("MoqErrorBind")
var ErrMoqErrorReject = fmt.Errorf("MoqErrorReject")
var ErrMoqErrorAlreadyResponded = fmt.Errorf("MoqErrorAlreadyResponded")
var ErrMoqErrorCodec = fmt.Errorf("MoqErrorCodec")
var ErrMoqErrorUnauthorized = fmt.Errorf("MoqErrorUnauthorized")
var ErrMoqErrorForbidden = fmt.Errorf("MoqErrorForbidden")
var ErrMoqErrorNotFound = fmt.Errorf("MoqErrorNotFound")
var ErrMoqErrorUnsupported = fmt.Errorf("MoqErrorUnsupported")
var ErrMoqErrorAlreadyCommitted = fmt.Errorf("MoqErrorAlreadyCommitted")
var ErrMoqErrorInvalidRoute = fmt.Errorf("MoqErrorInvalidRoute")
var ErrMoqErrorInvalidPattern = fmt.Errorf("MoqErrorInvalidPattern")
var ErrMoqErrorUnresolvableBroadcast = fmt.Errorf("MoqErrorUnresolvableBroadcast")
var ErrMoqErrorLog = fmt.Errorf("MoqErrorLog")

// Variant structs
// A protocol failure carrying the peer's session or stream code.
type MoqErrorProtocol struct {
	Details MoqProtocolError
}

// A protocol failure carrying the peer's session or stream code.
func NewMoqErrorProtocol(
	details MoqProtocolError,
) *MoqError {
	return &MoqError{err: &MoqErrorProtocol{
		Details: details}}
}

func (e MoqErrorProtocol) destroy() {
	FfiDestroyerMoqProtocolError{}.Destroy(e.Details)
}

func (err MoqErrorProtocol) Error() string {
	return fmt.Sprint("Protocol",
		": ",

		"Details=",
		err.Details,
	)
}

func (self MoqErrorProtocol) Is(target error) bool {
	return target == ErrMoqErrorProtocol
}

// The underlying QUIC/WebTransport connection failed.
type MoqErrorTransport struct {
	Field0 string
}

// The underlying QUIC/WebTransport connection failed.
func NewMoqErrorTransport(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorTransport{
		Field0: var0}}
}

func (e MoqErrorTransport) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorTransport) Error() string {
	return fmt.Sprint("Transport",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorTransport) Is(target error) bool {
	return target == ErrMoqErrorTransport
}

// A local failure without a session or stream code.
type MoqErrorInternal struct {
	Field0 string
}

// A local failure without a session or stream code.
func NewMoqErrorInternal(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorInternal{
		Field0: var0}}
}

func (e MoqErrorInternal) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorInternal) Error() string {
	return fmt.Sprint("Internal",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorInternal) Is(target error) bool {
	return target == ErrMoqErrorInternal
}

type MoqErrorMedia struct {
	Field0 string
}

func NewMoqErrorMedia(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorMedia{
		Field0: var0}}
}

func (e MoqErrorMedia) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorMedia) Error() string {
	return fmt.Sprint("Media",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorMedia) Is(target error) bool {
	return target == ErrMoqErrorMedia
}

type MoqErrorMux struct {
	Field0 string
}

func NewMoqErrorMux(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorMux{
		Field0: var0}}
}

func (e MoqErrorMux) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorMux) Error() string {
	return fmt.Sprint("Mux",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorMux) Is(target error) bool {
	return target == ErrMoqErrorMux
}

type MoqErrorJsonTrack struct {
	Field0 string
}

func NewMoqErrorJsonTrack(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorJsonTrack{
		Field0: var0}}
}

func (e MoqErrorJsonTrack) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorJsonTrack) Error() string {
	return fmt.Sprint("JsonTrack",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorJsonTrack) Is(target error) bool {
	return target == ErrMoqErrorJsonTrack
}

type MoqErrorAudio struct {
	Field0 string
}

func NewMoqErrorAudio(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorAudio{
		Field0: var0}}
}

func (e MoqErrorAudio) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorAudio) Error() string {
	return fmt.Sprint("Audio",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorAudio) Is(target error) bool {
	return target == ErrMoqErrorAudio
}

type MoqErrorVideo struct {
	Field0 string
}

func NewMoqErrorVideo(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorVideo{
		Field0: var0}}
}

func (e MoqErrorVideo) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorVideo) Error() string {
	return fmt.Sprint("Video",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorVideo) Is(target error) bool {
	return target == ErrMoqErrorVideo
}

type MoqErrorUrl struct {
	Field0 string
}

func NewMoqErrorUrl(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorUrl{
		Field0: var0}}
}

func (e MoqErrorUrl) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorUrl) Error() string {
	return fmt.Sprint("Url",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorUrl) Is(target error) bool {
	return target == ErrMoqErrorUrl
}

type MoqErrorTimeOverflow struct {
}

func NewMoqErrorTimeOverflow() *MoqError {
	return &MoqError{err: &MoqErrorTimeOverflow{}}
}

func (e MoqErrorTimeOverflow) destroy() {
}

func (err MoqErrorTimeOverflow) Error() string {
	return fmt.Sprint("TimeOverflow")
}

func (self MoqErrorTimeOverflow) Is(target error) bool {
	return target == ErrMoqErrorTimeOverflow
}

type MoqErrorLogLevel struct {
	Field0 string
}

func NewMoqErrorLogLevel(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorLogLevel{
		Field0: var0}}
}

func (e MoqErrorLogLevel) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorLogLevel) Error() string {
	return fmt.Sprint("LogLevel",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorLogLevel) Is(target error) bool {
	return target == ErrMoqErrorLogLevel
}

type MoqErrorTask struct {
	Field0 string
}

func NewMoqErrorTask(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorTask{
		Field0: var0}}
}

func (e MoqErrorTask) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorTask) Error() string {
	return fmt.Sprint("Task",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorTask) Is(target error) bool {
	return target == ErrMoqErrorTask
}

type MoqErrorJson struct {
	Field0 string
}

func NewMoqErrorJson(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorJson{
		Field0: var0}}
}

func (e MoqErrorJson) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorJson) Error() string {
	return fmt.Sprint("Json",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorJson) Is(target error) bool {
	return target == ErrMoqErrorJson
}

type MoqErrorCancelled struct {
}

func NewMoqErrorCancelled() *MoqError {
	return &MoqError{err: &MoqErrorCancelled{}}
}

func (e MoqErrorCancelled) destroy() {
}

func (err MoqErrorCancelled) Error() string {
	return fmt.Sprint("Cancelled")
}

func (self MoqErrorCancelled) Is(target error) bool {
	return target == ErrMoqErrorCancelled
}

type MoqErrorClosed struct {
}

func NewMoqErrorClosed() *MoqError {
	return &MoqError{err: &MoqErrorClosed{}}
}

func (e MoqErrorClosed) destroy() {
}

func (err MoqErrorClosed) Error() string {
	return fmt.Sprint("Closed")
}

func (self MoqErrorClosed) Is(target error) bool {
	return target == ErrMoqErrorClosed
}

// A configuration call lost the race with an in-flight async operation.
//
// The handle is still live: wait for the operation, then try again. A
// cancelled handle is [`Self::Cancelled`] instead.
type MoqErrorBusy struct {
}

// A configuration call lost the race with an in-flight async operation.
//
// The handle is still live: wait for the operation, then try again. A
// cancelled handle is [`Self::Cancelled`] instead.
func NewMoqErrorBusy() *MoqError {
	return &MoqError{err: &MoqErrorBusy{}}
}

func (e MoqErrorBusy) destroy() {
}

func (err MoqErrorBusy) Error() string {
	return fmt.Sprint("Busy")
}

func (self MoqErrorBusy) Is(target error) bool {
	return target == ErrMoqErrorBusy
}

type MoqErrorConnect struct {
	Field0 string
}

func NewMoqErrorConnect(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorConnect{
		Field0: var0}}
}

func (e MoqErrorConnect) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorConnect) Error() string {
	return fmt.Sprint("Connect",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorConnect) Is(target error) bool {
	return target == ErrMoqErrorConnect
}

type MoqErrorBind struct {
	Field0 string
}

func NewMoqErrorBind(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorBind{
		Field0: var0}}
}

func (e MoqErrorBind) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorBind) Error() string {
	return fmt.Sprint("Bind",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorBind) Is(target error) bool {
	return target == ErrMoqErrorBind
}

type MoqErrorReject struct {
	Field0 string
}

func NewMoqErrorReject(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorReject{
		Field0: var0}}
}

func (e MoqErrorReject) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorReject) Error() string {
	return fmt.Sprint("Reject",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorReject) Is(target error) bool {
	return target == ErrMoqErrorReject
}

type MoqErrorAlreadyResponded struct {
}

func NewMoqErrorAlreadyResponded() *MoqError {
	return &MoqError{err: &MoqErrorAlreadyResponded{}}
}

func (e MoqErrorAlreadyResponded) destroy() {
}

func (err MoqErrorAlreadyResponded) Error() string {
	return fmt.Sprint("AlreadyResponded")
}

func (self MoqErrorAlreadyResponded) Is(target error) bool {
	return target == ErrMoqErrorAlreadyResponded
}

type MoqErrorCodec struct {
	Field0 string
}

func NewMoqErrorCodec(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorCodec{
		Field0: var0}}
}

func (e MoqErrorCodec) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorCodec) Error() string {
	return fmt.Sprint("Codec",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorCodec) Is(target error) bool {
	return target == ErrMoqErrorCodec
}

type MoqErrorUnauthorized struct {
}

func NewMoqErrorUnauthorized() *MoqError {
	return &MoqError{err: &MoqErrorUnauthorized{}}
}

func (e MoqErrorUnauthorized) destroy() {
}

func (err MoqErrorUnauthorized) Error() string {
	return fmt.Sprint("Unauthorized")
}

func (self MoqErrorUnauthorized) Is(target error) bool {
	return target == ErrMoqErrorUnauthorized
}

type MoqErrorForbidden struct {
}

func NewMoqErrorForbidden() *MoqError {
	return &MoqError{err: &MoqErrorForbidden{}}
}

func (e MoqErrorForbidden) destroy() {
}

func (err MoqErrorForbidden) Error() string {
	return fmt.Sprint("Forbidden")
}

func (self MoqErrorForbidden) Is(target error) bool {
	return target == ErrMoqErrorForbidden
}

// The requested track or group is not available.
type MoqErrorNotFound struct {
}

// The requested track or group is not available.
func NewMoqErrorNotFound() *MoqError {
	return &MoqError{err: &MoqErrorNotFound{}}
}

func (e MoqErrorNotFound) destroy() {
}

func (err MoqErrorNotFound) Error() string {
	return fmt.Sprint("NotFound")
}

func (self MoqErrorNotFound) Is(target error) bool {
	return target == ErrMoqErrorNotFound
}

// The requested operation is not supported.
//
// A statement about this build or this peer, not about the call: the feature is
// unavailable however the caller asks for it. Caller misuse gets its own error, so
// that a binding can tell "MoQ can't do this here" from "you held it wrong".
type MoqErrorUnsupported struct {
}

// The requested operation is not supported.
//
// A statement about this build or this peer, not about the call: the feature is
// unavailable however the caller asks for it. Caller misuse gets its own error, so
// that a binding can tell "MoQ can't do this here" from "you held it wrong".
func NewMoqErrorUnsupported() *MoqError {
	return &MoqError{err: &MoqErrorUnsupported{}}
}

func (e MoqErrorUnsupported) destroy() {
}

func (err MoqErrorUnsupported) Error() string {
	return fmt.Sprint("Unsupported")
}

func (self MoqErrorUnsupported) Is(target error) bool {
	return target == ErrMoqErrorUnsupported
}

// This track already committed to the other delivery order.
//
// A track is read in arrival order or in sequence order, never both, and the first
// group read picks which. Reaching for the other one afterwards is this error rather
// than [`Self::Unsupported`]: both orders work fine here, the track just isn't
// reading in the one you asked for. Read the track through a second consumer if you
// genuinely need both.
type MoqErrorAlreadyCommitted struct {
}

// This track already committed to the other delivery order.
//
// A track is read in arrival order or in sequence order, never both, and the first
// group read picks which. Reaching for the other one afterwards is this error rather
// than [`Self::Unsupported`]: both orders work fine here, the track just isn't
// reading in the one you asked for. Read the track through a second consumer if you
// genuinely need both.
func NewMoqErrorAlreadyCommitted() *MoqError {
	return &MoqError{err: &MoqErrorAlreadyCommitted{}}
}

func (e MoqErrorAlreadyCommitted) destroy() {
}

func (err MoqErrorAlreadyCommitted) Error() string {
	return fmt.Sprint("AlreadyCommitted")
}

func (self MoqErrorAlreadyCommitted) Is(target error) bool {
	return target == ErrMoqErrorAlreadyCommitted
}

// A route carried an invalid hop id or too many hops.
type MoqErrorInvalidRoute struct {
	Field0 string
}

// A route carried an invalid hop id or too many hops.
func NewMoqErrorInvalidRoute(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorInvalidRoute{
		Field0: var0}}
}

func (e MoqErrorInvalidRoute) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorInvalidRoute) Error() string {
	return fmt.Sprint("InvalidRoute",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorInvalidRoute) Is(target error) bool {
	return target == ErrMoqErrorInvalidRoute
}

// A path pattern was empty, had a doubled slash, or used a reserved segment form.
type MoqErrorInvalidPattern struct {
	Field0 string
}

// A path pattern was empty, had a doubled slash, or used a reserved segment form.
func NewMoqErrorInvalidPattern(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorInvalidPattern{
		Field0: var0}}
}

func (e MoqErrorInvalidPattern) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorInvalidPattern) Error() string {
	return fmt.Sprint("InvalidPattern",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorInvalidPattern) Is(target error) bool {
	return target == ErrMoqErrorInvalidPattern
}

// A catalog rendition named another broadcast, but this consumer came from a standalone
// broadcast rather than an origin, so there is nothing to resolve the reference against.
type MoqErrorUnresolvableBroadcast struct {
	Field0 string
}

// A catalog rendition named another broadcast, but this consumer came from a standalone
// broadcast rather than an origin, so there is nothing to resolve the reference against.
func NewMoqErrorUnresolvableBroadcast(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorUnresolvableBroadcast{
		Field0: var0}}
}

func (e MoqErrorUnresolvableBroadcast) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorUnresolvableBroadcast) Error() string {
	return fmt.Sprint("UnresolvableBroadcast",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorUnresolvableBroadcast) Is(target error) bool {
	return target == ErrMoqErrorUnresolvableBroadcast
}

type MoqErrorLog struct {
	Field0 string
}

func NewMoqErrorLog(
	var0 string,
) *MoqError {
	return &MoqError{err: &MoqErrorLog{
		Field0: var0}}
}

func (e MoqErrorLog) destroy() {
	FfiDestroyerString{}.Destroy(e.Field0)
}

func (err MoqErrorLog) Error() string {
	return fmt.Sprint("Log",
		": ",

		"Field0=",
		err.Field0,
	)
}

func (self MoqErrorLog) Is(target error) bool {
	return target == ErrMoqErrorLog
}

type FfiConverterMoqError struct{}

var FfiConverterMoqErrorINSTANCE = FfiConverterMoqError{}

func (c FfiConverterMoqError) Lift(eb RustBufferI) *MoqError {
	return LiftFromRustBuffer[*MoqError](c, eb)
}

func (c FfiConverterMoqError) Lower(value *MoqError) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqError](c, value)
}

func (c FfiConverterMoqError) LowerExternal(value *MoqError) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqError](c, value))
}

func (c FfiConverterMoqError) Read(reader io.Reader) *MoqError {
	errorID := readUint32(reader)

	switch errorID {
	case 1:
		return &MoqError{&MoqErrorProtocol{
			Details: FfiConverterMoqProtocolErrorINSTANCE.Read(reader),
		}}
	case 2:
		return &MoqError{&MoqErrorTransport{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 3:
		return &MoqError{&MoqErrorInternal{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 4:
		return &MoqError{&MoqErrorMedia{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 5:
		return &MoqError{&MoqErrorMux{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 6:
		return &MoqError{&MoqErrorJsonTrack{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 7:
		return &MoqError{&MoqErrorAudio{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 8:
		return &MoqError{&MoqErrorVideo{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 9:
		return &MoqError{&MoqErrorUrl{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 10:
		return &MoqError{&MoqErrorTimeOverflow{}}
	case 11:
		return &MoqError{&MoqErrorLogLevel{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 12:
		return &MoqError{&MoqErrorTask{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 13:
		return &MoqError{&MoqErrorJson{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 14:
		return &MoqError{&MoqErrorCancelled{}}
	case 15:
		return &MoqError{&MoqErrorClosed{}}
	case 16:
		return &MoqError{&MoqErrorBusy{}}
	case 17:
		return &MoqError{&MoqErrorConnect{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 18:
		return &MoqError{&MoqErrorBind{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 19:
		return &MoqError{&MoqErrorReject{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 20:
		return &MoqError{&MoqErrorAlreadyResponded{}}
	case 21:
		return &MoqError{&MoqErrorCodec{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 22:
		return &MoqError{&MoqErrorUnauthorized{}}
	case 23:
		return &MoqError{&MoqErrorForbidden{}}
	case 24:
		return &MoqError{&MoqErrorNotFound{}}
	case 25:
		return &MoqError{&MoqErrorUnsupported{}}
	case 26:
		return &MoqError{&MoqErrorAlreadyCommitted{}}
	case 27:
		return &MoqError{&MoqErrorInvalidRoute{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 28:
		return &MoqError{&MoqErrorInvalidPattern{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 29:
		return &MoqError{&MoqErrorUnresolvableBroadcast{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 30:
		return &MoqError{&MoqErrorLog{
			Field0: FfiConverterStringINSTANCE.Read(reader),
		}}
	default:
		panic(fmt.Sprintf("Unknown error code %d in FfiConverterMoqError.Read()", errorID))
	}
}

func (c FfiConverterMoqError) Write(writer io.Writer, value *MoqError) {
	switch variantValue := value.err.(type) {
	case *MoqErrorProtocol:
		writeInt32(writer, 1)
		FfiConverterMoqProtocolErrorINSTANCE.Write(writer, variantValue.Details)
	case *MoqErrorTransport:
		writeInt32(writer, 2)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorInternal:
		writeInt32(writer, 3)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorMedia:
		writeInt32(writer, 4)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorMux:
		writeInt32(writer, 5)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorJsonTrack:
		writeInt32(writer, 6)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorAudio:
		writeInt32(writer, 7)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorVideo:
		writeInt32(writer, 8)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorUrl:
		writeInt32(writer, 9)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorTimeOverflow:
		writeInt32(writer, 10)
	case *MoqErrorLogLevel:
		writeInt32(writer, 11)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorTask:
		writeInt32(writer, 12)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorJson:
		writeInt32(writer, 13)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorCancelled:
		writeInt32(writer, 14)
	case *MoqErrorClosed:
		writeInt32(writer, 15)
	case *MoqErrorBusy:
		writeInt32(writer, 16)
	case *MoqErrorConnect:
		writeInt32(writer, 17)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorBind:
		writeInt32(writer, 18)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorReject:
		writeInt32(writer, 19)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorAlreadyResponded:
		writeInt32(writer, 20)
	case *MoqErrorCodec:
		writeInt32(writer, 21)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorUnauthorized:
		writeInt32(writer, 22)
	case *MoqErrorForbidden:
		writeInt32(writer, 23)
	case *MoqErrorNotFound:
		writeInt32(writer, 24)
	case *MoqErrorUnsupported:
		writeInt32(writer, 25)
	case *MoqErrorAlreadyCommitted:
		writeInt32(writer, 26)
	case *MoqErrorInvalidRoute:
		writeInt32(writer, 27)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorInvalidPattern:
		writeInt32(writer, 28)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorUnresolvableBroadcast:
		writeInt32(writer, 29)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	case *MoqErrorLog:
		writeInt32(writer, 30)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field0)
	default:
		_ = variantValue
		panic(fmt.Sprintf("invalid error value `%v` in FfiConverterMoqError.Write", value))
	}
}

type FfiDestroyerMoqError struct{}

func (_ FfiDestroyerMoqError) Destroy(value *MoqError) {
	switch variantValue := value.err.(type) {
	case MoqErrorProtocol:
		variantValue.destroy()
	case MoqErrorTransport:
		variantValue.destroy()
	case MoqErrorInternal:
		variantValue.destroy()
	case MoqErrorMedia:
		variantValue.destroy()
	case MoqErrorMux:
		variantValue.destroy()
	case MoqErrorJsonTrack:
		variantValue.destroy()
	case MoqErrorAudio:
		variantValue.destroy()
	case MoqErrorVideo:
		variantValue.destroy()
	case MoqErrorUrl:
		variantValue.destroy()
	case MoqErrorTimeOverflow:
		variantValue.destroy()
	case MoqErrorLogLevel:
		variantValue.destroy()
	case MoqErrorTask:
		variantValue.destroy()
	case MoqErrorJson:
		variantValue.destroy()
	case MoqErrorCancelled:
		variantValue.destroy()
	case MoqErrorClosed:
		variantValue.destroy()
	case MoqErrorBusy:
		variantValue.destroy()
	case MoqErrorConnect:
		variantValue.destroy()
	case MoqErrorBind:
		variantValue.destroy()
	case MoqErrorReject:
		variantValue.destroy()
	case MoqErrorAlreadyResponded:
		variantValue.destroy()
	case MoqErrorCodec:
		variantValue.destroy()
	case MoqErrorUnauthorized:
		variantValue.destroy()
	case MoqErrorForbidden:
		variantValue.destroy()
	case MoqErrorNotFound:
		variantValue.destroy()
	case MoqErrorUnsupported:
		variantValue.destroy()
	case MoqErrorAlreadyCommitted:
		variantValue.destroy()
	case MoqErrorInvalidRoute:
		variantValue.destroy()
	case MoqErrorInvalidPattern:
		variantValue.destroy()
	case MoqErrorUnresolvableBroadcast:
		variantValue.destroy()
	case MoqErrorLog:
		variantValue.destroy()
	default:
		_ = variantValue
		panic(fmt.Sprintf("invalid error value `%v` in FfiDestroyerMoqError.Destroy", value))
	}
}

// Which registry a protocol code belongs to. Session and stream codes are disjoint, so
// the same integer is a different failure in each.
type MoqErrorScope uint

const (
	// A session close code.
	MoqErrorScopeSession MoqErrorScope = 1
	// A stream reset or stop code.
	MoqErrorScopeStream MoqErrorScope = 2
)

type FfiConverterMoqErrorScope struct{}

var FfiConverterMoqErrorScopeINSTANCE = FfiConverterMoqErrorScope{}

func (c FfiConverterMoqErrorScope) Lift(rb RustBufferI) MoqErrorScope {
	return LiftFromRustBuffer[MoqErrorScope](c, rb)
}

func (c FfiConverterMoqErrorScope) Lower(value MoqErrorScope) C.RustBuffer {
	return LowerIntoRustBuffer[MoqErrorScope](c, value)
}

func (c FfiConverterMoqErrorScope) LowerExternal(value MoqErrorScope) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqErrorScope](c, value))
}
func (FfiConverterMoqErrorScope) Read(reader io.Reader) MoqErrorScope {
	id := readInt32(reader)
	return MoqErrorScope(id)
}

func (FfiConverterMoqErrorScope) Write(writer io.Writer, value MoqErrorScope) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerMoqErrorScope struct{}

func (_ FfiDestroyerMoqErrorScope) Destroy(value MoqErrorScope) {
}

// A recognized protocol kind, or [`Self::App`] / [`Self::Unknown`] when the code is not
// one of the named ones. Pair with [`MoqErrorScope`]: `Cancel` is 0 on a session and 1
// on a stream.
type MoqProtocolKind uint

const (
	// Ending normally, with no error. Session 0, stream 1.
	MoqProtocolKindCancel MoqProtocolKind = 1
	// Something went wrong that isn't worth a dedicated code. Session 1, stream 0.
	MoqProtocolKindInternal MoqProtocolKind = 2
	// The peer's token does not grant the requested path or operation.
	MoqProtocolKindUnauthorized MoqProtocolKind = 3
	// The peer broke a protocol rule; the session is unusable.
	MoqProtocolKindProtocolViolation MoqProtocolKind = 4
	// A key-value pair was malformed or repeated more than allowed.
	MoqProtocolKindKeyValueFormatting MoqProtocolKind = 5
	// The peer did not close within the GOAWAY drain deadline.
	MoqProtocolKindGoawayTimeout MoqProtocolKind = 6
	// A control message took too long.
	MoqProtocolKindTimeout MoqProtocolKind = 7
	// No version could be negotiated.
	MoqProtocolKindVersion MoqProtocolKind = 8
	// The content missed its delivery deadline.
	MoqProtocolKindDeliveryTimeout MoqProtocolKind = 9
	// The session ended, taking this stream with it.
	MoqProtocolKindSessionClosed MoqProtocolKind = 10
	// The session is going away (a GOAWAY was received).
	MoqProtocolKindGoingAway MoqProtocolKind = 11
	// The reader fell too far behind and content was dropped to catch up.
	MoqProtocolKindTooFarBehind MoqProtocolKind = 12
	// The track's content could not be parsed.
	MoqProtocolKindMalformedTrack MoqProtocolKind = 13
	// The requested broadcast or track does not exist at the peer.
	MoqProtocolKindNotFound MoqProtocolKind = 14
	// The broadcast is neither announced nor served, so there is no route to it.
	MoqProtocolKindUnroutable MoqProtocolKind = 15
	// The group was superseded by a newer group and dropped.
	MoqProtocolKindOld MoqProtocolKind = 16
	// The group was dropped under memory pressure.
	MoqProtocolKindEvicted MoqProtocolKind = 17
	// A frame's payload length disagreed with its declared size.
	MoqProtocolKindWrongSize MoqProtocolKind = 18
	// A frame declared a payload larger than the receiver accepts.
	MoqProtocolKindFrameTooLarge MoqProtocolKind = 19
	// A frame's timestamp doesn't match its track's negotiated timescale.
	MoqProtocolKindTimestampMismatch MoqProtocolKind = 20
	// An application-chosen code, offset into the 64+ range on the wire.
	MoqProtocolKindApp MoqProtocolKind = 21
	// A code this version does not recognize; [`MoqProtocolError::code`] is the value.
	MoqProtocolKindUnknown MoqProtocolKind = 22
)

type FfiConverterMoqProtocolKind struct{}

var FfiConverterMoqProtocolKindINSTANCE = FfiConverterMoqProtocolKind{}

func (c FfiConverterMoqProtocolKind) Lift(rb RustBufferI) MoqProtocolKind {
	return LiftFromRustBuffer[MoqProtocolKind](c, rb)
}

func (c FfiConverterMoqProtocolKind) Lower(value MoqProtocolKind) C.RustBuffer {
	return LowerIntoRustBuffer[MoqProtocolKind](c, value)
}

func (c FfiConverterMoqProtocolKind) LowerExternal(value MoqProtocolKind) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqProtocolKind](c, value))
}
func (FfiConverterMoqProtocolKind) Read(reader io.Reader) MoqProtocolKind {
	id := readInt32(reader)
	return MoqProtocolKind(id)
}

func (FfiConverterMoqProtocolKind) Write(writer io.Writer, value MoqProtocolKind) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerMoqProtocolKind struct{}

func (_ FfiDestroyerMoqProtocolKind) Destroy(value MoqProtocolKind) {
}

// The network transport carrying an incoming session.
type MoqTransport uint

const (
	// QUIC, either directly or through WebTransport over HTTP/3.
	MoqTransportQuic MoqTransport = 1
	// An Iroh QUIC connection.
	MoqTransportIroh MoqTransport = 2
	// A WebSocket connection using qmux framing.
	MoqTransportWebSocket MoqTransport = 3
	// A plaintext TCP connection using qmux framing.
	MoqTransportTcp MoqTransport = 4
	// A Unix domain socket using qmux framing.
	MoqTransportUnix MoqTransport = 5
)

type FfiConverterMoqTransport struct{}

var FfiConverterMoqTransportINSTANCE = FfiConverterMoqTransport{}

func (c FfiConverterMoqTransport) Lift(rb RustBufferI) MoqTransport {
	return LiftFromRustBuffer[MoqTransport](c, rb)
}

func (c FfiConverterMoqTransport) Lower(value MoqTransport) C.RustBuffer {
	return LowerIntoRustBuffer[MoqTransport](c, value)
}

func (c FfiConverterMoqTransport) LowerExternal(value MoqTransport) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqTransport](c, value))
}
func (FfiConverterMoqTransport) Read(reader io.Reader) MoqTransport {
	id := readInt32(reader)
	return MoqTransport(id)
}

func (FfiConverterMoqTransport) Write(writer io.Writer, value MoqTransport) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerMoqTransport struct{}

func (_ FfiDestroyerMoqTransport) Destroy(value MoqTransport) {
}

// Output video codec.
//
// Not every codec has a backend on every machine: H.265 is hardware-only, so
// publishing it fails where no hardware encoder is available.
type MoqVideoCodec uint

const (
	// H.264 / AVC, published as an `avc3` track.
	MoqVideoCodecH264 MoqVideoCodec = 1
	// H.265 / HEVC, published as a `hev1` track.
	MoqVideoCodecH265 MoqVideoCodec = 2
)

type FfiConverterMoqVideoCodec struct{}

var FfiConverterMoqVideoCodecINSTANCE = FfiConverterMoqVideoCodec{}

func (c FfiConverterMoqVideoCodec) Lift(rb RustBufferI) MoqVideoCodec {
	return LiftFromRustBuffer[MoqVideoCodec](c, rb)
}

func (c FfiConverterMoqVideoCodec) Lower(value MoqVideoCodec) C.RustBuffer {
	return LowerIntoRustBuffer[MoqVideoCodec](c, value)
}

func (c FfiConverterMoqVideoCodec) LowerExternal(value MoqVideoCodec) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqVideoCodec](c, value))
}
func (FfiConverterMoqVideoCodec) Read(reader io.Reader) MoqVideoCodec {
	id := readInt32(reader)
	return MoqVideoCodec(id)
}

func (FfiConverterMoqVideoCodec) Write(writer io.Writer, value MoqVideoCodec) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerMoqVideoCodec struct{}

func (_ FfiDestroyerMoqVideoCodec) Destroy(value MoqVideoCodec) {
}

// Which encoder implementation to use.
//
// These bindings compile VideoToolbox (macOS), Media Foundation (Windows),
// openh264 (software, everywhere), and on Linux NVENC and VAAPI, which dlopen
// their driver at runtime and drop out of `Auto` when it is absent.
type MoqVideoEncoderKind interface {
	Destroy()
}

// Prefer a platform hardware encoder, falling back to software.
type MoqVideoEncoderKindAuto struct {
}

func (e MoqVideoEncoderKindAuto) Destroy() {
}

// Hardware only; fails if none is available.
type MoqVideoEncoderKindHardware struct {
}

func (e MoqVideoEncoderKindHardware) Destroy() {
}

// Software only (openh264, H.264 only).
type MoqVideoEncoderKindSoftware struct {
}

func (e MoqVideoEncoderKindSoftware) Destroy() {
}

// A specific backend that moq-ffi compiles: `"videotoolbox"` (macOS),
// `"mediafoundation"` (Windows), `"nvenc"` / `"vaapi"` (Linux), or
// `"openh264"` (software, everywhere).
// Naming one this build lacks fails with a no-encoder error, so reach for
// this only when [`Auto`](Self::Auto) picks the wrong one.
type MoqVideoEncoderKindNamed struct {
	Name string
}

func (e MoqVideoEncoderKindNamed) Destroy() {
	FfiDestroyerString{}.Destroy(e.Name)
}

type FfiConverterMoqVideoEncoderKind struct{}

var FfiConverterMoqVideoEncoderKindINSTANCE = FfiConverterMoqVideoEncoderKind{}

func (c FfiConverterMoqVideoEncoderKind) Lift(rb RustBufferI) MoqVideoEncoderKind {
	return LiftFromRustBuffer[MoqVideoEncoderKind](c, rb)
}

func (c FfiConverterMoqVideoEncoderKind) Lower(value MoqVideoEncoderKind) C.RustBuffer {
	return LowerIntoRustBuffer[MoqVideoEncoderKind](c, value)
}

func (c FfiConverterMoqVideoEncoderKind) LowerExternal(value MoqVideoEncoderKind) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqVideoEncoderKind](c, value))
}
func (FfiConverterMoqVideoEncoderKind) Read(reader io.Reader) MoqVideoEncoderKind {
	id := readInt32(reader)
	switch id {
	case 1:
		return MoqVideoEncoderKindAuto{}
	case 2:
		return MoqVideoEncoderKindHardware{}
	case 3:
		return MoqVideoEncoderKindSoftware{}
	case 4:
		return MoqVideoEncoderKindNamed{
			FfiConverterStringINSTANCE.Read(reader),
		}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterMoqVideoEncoderKind.Read()", id))
	}
}

func (FfiConverterMoqVideoEncoderKind) Write(writer io.Writer, value MoqVideoEncoderKind) {
	switch variant_value := value.(type) {
	case MoqVideoEncoderKindAuto:
		writeInt32(writer, 1)
	case MoqVideoEncoderKindHardware:
		writeInt32(writer, 2)
	case MoqVideoEncoderKindSoftware:
		writeInt32(writer, 3)
	case MoqVideoEncoderKindNamed:
		writeInt32(writer, 4)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Name)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterMoqVideoEncoderKind.Write", value))
	}
}

type FfiDestroyerMoqVideoEncoderKind struct{}

func (_ FfiDestroyerMoqVideoEncoderKind) Destroy(value MoqVideoEncoderKind) {
	value.Destroy()
}

// A single video codec an importer can parse.
//
// H.264 and H.265 appear twice each because the framing differs, not just the codec: `Avc1`/`Hvc1`
// are length-prefixed with an out-of-band config record, while `Avc3`/`Hev1` are Annex-B with the
// parameter sets inline.
type MoqVideoFormat uint

const (
	// H.264, length-prefixed NALUs with an out-of-band avcC.
	MoqVideoFormatAvc1 MoqVideoFormat = 1
	// H.264, Annex-B with inline SPS/PPS.
	MoqVideoFormatAvc3 MoqVideoFormat = 2
	// H.265, length-prefixed NALUs with an out-of-band hvcC.
	MoqVideoFormatHvc1 MoqVideoFormat = 3
	// H.265, Annex-B with inline parameter sets.
	MoqVideoFormatHev1 MoqVideoFormat = 4
	// AV1.
	MoqVideoFormatAv01 MoqVideoFormat = 5
	// VP8.
	MoqVideoFormatVp8 MoqVideoFormat = 6
	// VP9.
	MoqVideoFormatVp9 MoqVideoFormat = 7
)

type FfiConverterMoqVideoFormat struct{}

var FfiConverterMoqVideoFormatINSTANCE = FfiConverterMoqVideoFormat{}

func (c FfiConverterMoqVideoFormat) Lift(rb RustBufferI) MoqVideoFormat {
	return LiftFromRustBuffer[MoqVideoFormat](c, rb)
}

func (c FfiConverterMoqVideoFormat) Lower(value MoqVideoFormat) C.RustBuffer {
	return LowerIntoRustBuffer[MoqVideoFormat](c, value)
}

func (c FfiConverterMoqVideoFormat) LowerExternal(value MoqVideoFormat) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqVideoFormat](c, value))
}
func (FfiConverterMoqVideoFormat) Read(reader io.Reader) MoqVideoFormat {
	id := readInt32(reader)
	return MoqVideoFormat(id)
}

func (FfiConverterMoqVideoFormat) Write(writer io.Writer, value MoqVideoFormat) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerMoqVideoFormat struct{}

func (_ FfiDestroyerMoqVideoFormat) Destroy(value MoqVideoFormat) {
}

// A CPU pixel layout: what [`MoqVideoProducer::write`] is fed, and what
// [`MoqBroadcastConsumer::decode_video`] hands back.
type MoqVideoPixelFormat uint

const (
	// Tightly-packed planar I420: Y, then U, then V, no row padding
	// (`width * height * 3 / 2` bytes).
	MoqVideoPixelFormatI420 MoqVideoPixelFormat = 1
	// Tightly-packed RGBA, `width * height * 4` bytes, no row padding.
	MoqVideoPixelFormatRgba MoqVideoPixelFormat = 2
)

type FfiConverterMoqVideoPixelFormat struct{}

var FfiConverterMoqVideoPixelFormatINSTANCE = FfiConverterMoqVideoPixelFormat{}

func (c FfiConverterMoqVideoPixelFormat) Lift(rb RustBufferI) MoqVideoPixelFormat {
	return LiftFromRustBuffer[MoqVideoPixelFormat](c, rb)
}

func (c FfiConverterMoqVideoPixelFormat) Lower(value MoqVideoPixelFormat) C.RustBuffer {
	return LowerIntoRustBuffer[MoqVideoPixelFormat](c, value)
}

func (c FfiConverterMoqVideoPixelFormat) LowerExternal(value MoqVideoPixelFormat) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqVideoPixelFormat](c, value))
}
func (FfiConverterMoqVideoPixelFormat) Read(reader io.Reader) MoqVideoPixelFormat {
	id := readInt32(reader)
	return MoqVideoPixelFormat(id)
}

func (FfiConverterMoqVideoPixelFormat) Write(writer io.Writer, value MoqVideoPixelFormat) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerMoqVideoPixelFormat struct{}

func (_ FfiDestroyerMoqVideoPixelFormat) Destroy(value MoqVideoPixelFormat) {
}

type FfiConverterOptionalUint32 struct{}

var FfiConverterOptionalUint32INSTANCE = FfiConverterOptionalUint32{}

func (c FfiConverterOptionalUint32) Lift(rb RustBufferI) *uint32 {
	return LiftFromRustBuffer[*uint32](c, rb)
}

func (_ FfiConverterOptionalUint32) Read(reader io.Reader) *uint32 {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterUint32INSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalUint32) Lower(value *uint32) C.RustBuffer {
	return LowerIntoRustBuffer[*uint32](c, value)
}

func (c FfiConverterOptionalUint32) LowerExternal(value *uint32) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*uint32](c, value))
}

func (_ FfiConverterOptionalUint32) Write(writer io.Writer, value *uint32) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterUint32INSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalUint32 struct{}

func (_ FfiDestroyerOptionalUint32) Destroy(value *uint32) {
	if value != nil {
		FfiDestroyerUint32{}.Destroy(*value)
	}
}

type FfiConverterOptionalUint64 struct{}

var FfiConverterOptionalUint64INSTANCE = FfiConverterOptionalUint64{}

func (c FfiConverterOptionalUint64) Lift(rb RustBufferI) *uint64 {
	return LiftFromRustBuffer[*uint64](c, rb)
}

func (_ FfiConverterOptionalUint64) Read(reader io.Reader) *uint64 {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterUint64INSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalUint64) Lower(value *uint64) C.RustBuffer {
	return LowerIntoRustBuffer[*uint64](c, value)
}

func (c FfiConverterOptionalUint64) LowerExternal(value *uint64) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*uint64](c, value))
}

func (_ FfiConverterOptionalUint64) Write(writer io.Writer, value *uint64) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterUint64INSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalUint64 struct{}

func (_ FfiDestroyerOptionalUint64) Destroy(value *uint64) {
	if value != nil {
		FfiDestroyerUint64{}.Destroy(*value)
	}
}

type FfiConverterOptionalFloat64 struct{}

var FfiConverterOptionalFloat64INSTANCE = FfiConverterOptionalFloat64{}

func (c FfiConverterOptionalFloat64) Lift(rb RustBufferI) *float64 {
	return LiftFromRustBuffer[*float64](c, rb)
}

func (_ FfiConverterOptionalFloat64) Read(reader io.Reader) *float64 {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterFloat64INSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalFloat64) Lower(value *float64) C.RustBuffer {
	return LowerIntoRustBuffer[*float64](c, value)
}

func (c FfiConverterOptionalFloat64) LowerExternal(value *float64) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*float64](c, value))
}

func (_ FfiConverterOptionalFloat64) Write(writer io.Writer, value *float64) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterFloat64INSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalFloat64 struct{}

func (_ FfiDestroyerOptionalFloat64) Destroy(value *float64) {
	if value != nil {
		FfiDestroyerFloat64{}.Destroy(*value)
	}
}

type FfiConverterOptionalBool struct{}

var FfiConverterOptionalBoolINSTANCE = FfiConverterOptionalBool{}

func (c FfiConverterOptionalBool) Lift(rb RustBufferI) *bool {
	return LiftFromRustBuffer[*bool](c, rb)
}

func (_ FfiConverterOptionalBool) Read(reader io.Reader) *bool {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterBoolINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalBool) Lower(value *bool) C.RustBuffer {
	return LowerIntoRustBuffer[*bool](c, value)
}

func (c FfiConverterOptionalBool) LowerExternal(value *bool) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*bool](c, value))
}

func (_ FfiConverterOptionalBool) Write(writer io.Writer, value *bool) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterBoolINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalBool struct{}

func (_ FfiDestroyerOptionalBool) Destroy(value *bool) {
	if value != nil {
		FfiDestroyerBool{}.Destroy(*value)
	}
}

type FfiConverterOptionalString struct{}

var FfiConverterOptionalStringINSTANCE = FfiConverterOptionalString{}

func (c FfiConverterOptionalString) Lift(rb RustBufferI) *string {
	return LiftFromRustBuffer[*string](c, rb)
}

func (_ FfiConverterOptionalString) Read(reader io.Reader) *string {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterStringINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalString) Lower(value *string) C.RustBuffer {
	return LowerIntoRustBuffer[*string](c, value)
}

func (c FfiConverterOptionalString) LowerExternal(value *string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*string](c, value))
}

func (_ FfiConverterOptionalString) Write(writer io.Writer, value *string) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalString struct{}

func (_ FfiDestroyerOptionalString) Destroy(value *string) {
	if value != nil {
		FfiDestroyerString{}.Destroy(*value)
	}
}

type FfiConverterOptionalBytes struct{}

var FfiConverterOptionalBytesINSTANCE = FfiConverterOptionalBytes{}

func (c FfiConverterOptionalBytes) Lift(rb RustBufferI) *[]byte {
	return LiftFromRustBuffer[*[]byte](c, rb)
}

func (_ FfiConverterOptionalBytes) Read(reader io.Reader) *[]byte {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterBytesINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalBytes) Lower(value *[]byte) C.RustBuffer {
	return LowerIntoRustBuffer[*[]byte](c, value)
}

func (c FfiConverterOptionalBytes) LowerExternal(value *[]byte) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*[]byte](c, value))
}

func (_ FfiConverterOptionalBytes) Write(writer io.Writer, value *[]byte) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterBytesINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalBytes struct{}

func (_ FfiDestroyerOptionalBytes) Destroy(value *[]byte) {
	if value != nil {
		FfiDestroyerBytes{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqAnnounceUpdate struct{}

var FfiConverterOptionalMoqAnnounceUpdateINSTANCE = FfiConverterOptionalMoqAnnounceUpdate{}

func (c FfiConverterOptionalMoqAnnounceUpdate) Lift(rb RustBufferI) **MoqAnnounceUpdate {
	return LiftFromRustBuffer[**MoqAnnounceUpdate](c, rb)
}

func (_ FfiConverterOptionalMoqAnnounceUpdate) Read(reader io.Reader) **MoqAnnounceUpdate {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqAnnounceUpdateINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqAnnounceUpdate) Lower(value **MoqAnnounceUpdate) C.RustBuffer {
	return LowerIntoRustBuffer[**MoqAnnounceUpdate](c, value)
}

func (c FfiConverterOptionalMoqAnnounceUpdate) LowerExternal(value **MoqAnnounceUpdate) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[**MoqAnnounceUpdate](c, value))
}

func (_ FfiConverterOptionalMoqAnnounceUpdate) Write(writer io.Writer, value **MoqAnnounceUpdate) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqAnnounceUpdateINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqAnnounceUpdate struct{}

func (_ FfiDestroyerOptionalMoqAnnounceUpdate) Destroy(value **MoqAnnounceUpdate) {
	if value != nil {
		FfiDestroyerMoqAnnounceUpdate{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqBandwidth struct{}

var FfiConverterOptionalMoqBandwidthINSTANCE = FfiConverterOptionalMoqBandwidth{}

func (c FfiConverterOptionalMoqBandwidth) Lift(rb RustBufferI) **MoqBandwidth {
	return LiftFromRustBuffer[**MoqBandwidth](c, rb)
}

func (_ FfiConverterOptionalMoqBandwidth) Read(reader io.Reader) **MoqBandwidth {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqBandwidthINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqBandwidth) Lower(value **MoqBandwidth) C.RustBuffer {
	return LowerIntoRustBuffer[**MoqBandwidth](c, value)
}

func (c FfiConverterOptionalMoqBandwidth) LowerExternal(value **MoqBandwidth) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[**MoqBandwidth](c, value))
}

func (_ FfiConverterOptionalMoqBandwidth) Write(writer io.Writer, value **MoqBandwidth) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqBandwidthINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqBandwidth struct{}

func (_ FfiDestroyerOptionalMoqBandwidth) Destroy(value **MoqBandwidth) {
	if value != nil {
		FfiDestroyerMoqBandwidth{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqGroupConsumer struct{}

var FfiConverterOptionalMoqGroupConsumerINSTANCE = FfiConverterOptionalMoqGroupConsumer{}

func (c FfiConverterOptionalMoqGroupConsumer) Lift(rb RustBufferI) **MoqGroupConsumer {
	return LiftFromRustBuffer[**MoqGroupConsumer](c, rb)
}

func (_ FfiConverterOptionalMoqGroupConsumer) Read(reader io.Reader) **MoqGroupConsumer {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqGroupConsumerINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqGroupConsumer) Lower(value **MoqGroupConsumer) C.RustBuffer {
	return LowerIntoRustBuffer[**MoqGroupConsumer](c, value)
}

func (c FfiConverterOptionalMoqGroupConsumer) LowerExternal(value **MoqGroupConsumer) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[**MoqGroupConsumer](c, value))
}

func (_ FfiConverterOptionalMoqGroupConsumer) Write(writer io.Writer, value **MoqGroupConsumer) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqGroupConsumerINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqGroupConsumer struct{}

func (_ FfiDestroyerOptionalMoqGroupConsumer) Destroy(value **MoqGroupConsumer) {
	if value != nil {
		FfiDestroyerMoqGroupConsumer{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqOriginProducer struct{}

var FfiConverterOptionalMoqOriginProducerINSTANCE = FfiConverterOptionalMoqOriginProducer{}

func (c FfiConverterOptionalMoqOriginProducer) Lift(rb RustBufferI) **MoqOriginProducer {
	return LiftFromRustBuffer[**MoqOriginProducer](c, rb)
}

func (_ FfiConverterOptionalMoqOriginProducer) Read(reader io.Reader) **MoqOriginProducer {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqOriginProducerINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqOriginProducer) Lower(value **MoqOriginProducer) C.RustBuffer {
	return LowerIntoRustBuffer[**MoqOriginProducer](c, value)
}

func (c FfiConverterOptionalMoqOriginProducer) LowerExternal(value **MoqOriginProducer) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[**MoqOriginProducer](c, value))
}

func (_ FfiConverterOptionalMoqOriginProducer) Write(writer io.Writer, value **MoqOriginProducer) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqOriginProducerINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqOriginProducer struct{}

func (_ FfiDestroyerOptionalMoqOriginProducer) Destroy(value **MoqOriginProducer) {
	if value != nil {
		FfiDestroyerMoqOriginProducer{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqRequest struct{}

var FfiConverterOptionalMoqRequestINSTANCE = FfiConverterOptionalMoqRequest{}

func (c FfiConverterOptionalMoqRequest) Lift(rb RustBufferI) **MoqRequest {
	return LiftFromRustBuffer[**MoqRequest](c, rb)
}

func (_ FfiConverterOptionalMoqRequest) Read(reader io.Reader) **MoqRequest {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqRequestINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqRequest) Lower(value **MoqRequest) C.RustBuffer {
	return LowerIntoRustBuffer[**MoqRequest](c, value)
}

func (c FfiConverterOptionalMoqRequest) LowerExternal(value **MoqRequest) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[**MoqRequest](c, value))
}

func (_ FfiConverterOptionalMoqRequest) Write(writer io.Writer, value **MoqRequest) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqRequestINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqRequest struct{}

func (_ FfiDestroyerOptionalMoqRequest) Destroy(value **MoqRequest) {
	if value != nil {
		FfiDestroyerMoqRequest{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqReservation struct{}

var FfiConverterOptionalMoqReservationINSTANCE = FfiConverterOptionalMoqReservation{}

func (c FfiConverterOptionalMoqReservation) Lift(rb RustBufferI) **MoqReservation {
	return LiftFromRustBuffer[**MoqReservation](c, rb)
}

func (_ FfiConverterOptionalMoqReservation) Read(reader io.Reader) **MoqReservation {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqReservationINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqReservation) Lower(value **MoqReservation) C.RustBuffer {
	return LowerIntoRustBuffer[**MoqReservation](c, value)
}

func (c FfiConverterOptionalMoqReservation) LowerExternal(value **MoqReservation) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[**MoqReservation](c, value))
}

func (_ FfiConverterOptionalMoqReservation) Write(writer io.Writer, value **MoqReservation) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqReservationINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqReservation struct{}

func (_ FfiDestroyerOptionalMoqReservation) Destroy(value **MoqReservation) {
	if value != nil {
		FfiDestroyerMoqReservation{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqAudioFrame struct{}

var FfiConverterOptionalMoqAudioFrameINSTANCE = FfiConverterOptionalMoqAudioFrame{}

func (c FfiConverterOptionalMoqAudioFrame) Lift(rb RustBufferI) *MoqAudioFrame {
	return LiftFromRustBuffer[*MoqAudioFrame](c, rb)
}

func (_ FfiConverterOptionalMoqAudioFrame) Read(reader io.Reader) *MoqAudioFrame {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqAudioFrameINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqAudioFrame) Lower(value *MoqAudioFrame) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqAudioFrame](c, value)
}

func (c FfiConverterOptionalMoqAudioFrame) LowerExternal(value *MoqAudioFrame) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqAudioFrame](c, value))
}

func (_ FfiConverterOptionalMoqAudioFrame) Write(writer io.Writer, value *MoqAudioFrame) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqAudioFrameINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqAudioFrame struct{}

func (_ FfiDestroyerOptionalMoqAudioFrame) Destroy(value *MoqAudioFrame) {
	if value != nil {
		FfiDestroyerMoqAudioFrame{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqCatalog struct{}

var FfiConverterOptionalMoqCatalogINSTANCE = FfiConverterOptionalMoqCatalog{}

func (c FfiConverterOptionalMoqCatalog) Lift(rb RustBufferI) *MoqCatalog {
	return LiftFromRustBuffer[*MoqCatalog](c, rb)
}

func (_ FfiConverterOptionalMoqCatalog) Read(reader io.Reader) *MoqCatalog {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqCatalogINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqCatalog) Lower(value *MoqCatalog) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqCatalog](c, value)
}

func (c FfiConverterOptionalMoqCatalog) LowerExternal(value *MoqCatalog) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqCatalog](c, value))
}

func (_ FfiConverterOptionalMoqCatalog) Write(writer io.Writer, value *MoqCatalog) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqCatalogINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqCatalog struct{}

func (_ FfiDestroyerOptionalMoqCatalog) Destroy(value *MoqCatalog) {
	if value != nil {
		FfiDestroyerMoqCatalog{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqDatagram struct{}

var FfiConverterOptionalMoqDatagramINSTANCE = FfiConverterOptionalMoqDatagram{}

func (c FfiConverterOptionalMoqDatagram) Lift(rb RustBufferI) *MoqDatagram {
	return LiftFromRustBuffer[*MoqDatagram](c, rb)
}

func (_ FfiConverterOptionalMoqDatagram) Read(reader io.Reader) *MoqDatagram {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqDatagramINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqDatagram) Lower(value *MoqDatagram) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqDatagram](c, value)
}

func (c FfiConverterOptionalMoqDatagram) LowerExternal(value *MoqDatagram) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqDatagram](c, value))
}

func (_ FfiConverterOptionalMoqDatagram) Write(writer io.Writer, value *MoqDatagram) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqDatagramINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqDatagram struct{}

func (_ FfiDestroyerOptionalMoqDatagram) Destroy(value *MoqDatagram) {
	if value != nil {
		FfiDestroyerMoqDatagram{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqDimensions struct{}

var FfiConverterOptionalMoqDimensionsINSTANCE = FfiConverterOptionalMoqDimensions{}

func (c FfiConverterOptionalMoqDimensions) Lift(rb RustBufferI) *MoqDimensions {
	return LiftFromRustBuffer[*MoqDimensions](c, rb)
}

func (_ FfiConverterOptionalMoqDimensions) Read(reader io.Reader) *MoqDimensions {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqDimensionsINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqDimensions) Lower(value *MoqDimensions) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqDimensions](c, value)
}

func (c FfiConverterOptionalMoqDimensions) LowerExternal(value *MoqDimensions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqDimensions](c, value))
}

func (_ FfiConverterOptionalMoqDimensions) Write(writer io.Writer, value *MoqDimensions) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqDimensionsINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqDimensions struct{}

func (_ FfiDestroyerOptionalMoqDimensions) Destroy(value *MoqDimensions) {
	if value != nil {
		FfiDestroyerMoqDimensions{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqFetchGroupOptions struct{}

var FfiConverterOptionalMoqFetchGroupOptionsINSTANCE = FfiConverterOptionalMoqFetchGroupOptions{}

func (c FfiConverterOptionalMoqFetchGroupOptions) Lift(rb RustBufferI) *MoqFetchGroupOptions {
	return LiftFromRustBuffer[*MoqFetchGroupOptions](c, rb)
}

func (_ FfiConverterOptionalMoqFetchGroupOptions) Read(reader io.Reader) *MoqFetchGroupOptions {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqFetchGroupOptionsINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqFetchGroupOptions) Lower(value *MoqFetchGroupOptions) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqFetchGroupOptions](c, value)
}

func (c FfiConverterOptionalMoqFetchGroupOptions) LowerExternal(value *MoqFetchGroupOptions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqFetchGroupOptions](c, value))
}

func (_ FfiConverterOptionalMoqFetchGroupOptions) Write(writer io.Writer, value *MoqFetchGroupOptions) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqFetchGroupOptionsINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqFetchGroupOptions struct{}

func (_ FfiDestroyerOptionalMoqFetchGroupOptions) Destroy(value *MoqFetchGroupOptions) {
	if value != nil {
		FfiDestroyerMoqFetchGroupOptions{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqFrame struct{}

var FfiConverterOptionalMoqFrameINSTANCE = FfiConverterOptionalMoqFrame{}

func (c FfiConverterOptionalMoqFrame) Lift(rb RustBufferI) *MoqFrame {
	return LiftFromRustBuffer[*MoqFrame](c, rb)
}

func (_ FfiConverterOptionalMoqFrame) Read(reader io.Reader) *MoqFrame {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqFrameINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqFrame) Lower(value *MoqFrame) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqFrame](c, value)
}

func (c FfiConverterOptionalMoqFrame) LowerExternal(value *MoqFrame) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqFrame](c, value))
}

func (_ FfiConverterOptionalMoqFrame) Write(writer io.Writer, value *MoqFrame) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqFrameINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqFrame struct{}

func (_ FfiDestroyerOptionalMoqFrame) Destroy(value *MoqFrame) {
	if value != nil {
		FfiDestroyerMoqFrame{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqMediaFrame struct{}

var FfiConverterOptionalMoqMediaFrameINSTANCE = FfiConverterOptionalMoqMediaFrame{}

func (c FfiConverterOptionalMoqMediaFrame) Lift(rb RustBufferI) *MoqMediaFrame {
	return LiftFromRustBuffer[*MoqMediaFrame](c, rb)
}

func (_ FfiConverterOptionalMoqMediaFrame) Read(reader io.Reader) *MoqMediaFrame {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqMediaFrameINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqMediaFrame) Lower(value *MoqMediaFrame) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqMediaFrame](c, value)
}

func (c FfiConverterOptionalMoqMediaFrame) LowerExternal(value *MoqMediaFrame) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqMediaFrame](c, value))
}

func (_ FfiConverterOptionalMoqMediaFrame) Write(writer io.Writer, value *MoqMediaFrame) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqMediaFrameINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqMediaFrame struct{}

func (_ FfiDestroyerOptionalMoqMediaFrame) Destroy(value *MoqMediaFrame) {
	if value != nil {
		FfiDestroyerMoqMediaFrame{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqSubscription struct{}

var FfiConverterOptionalMoqSubscriptionINSTANCE = FfiConverterOptionalMoqSubscription{}

func (c FfiConverterOptionalMoqSubscription) Lift(rb RustBufferI) *MoqSubscription {
	return LiftFromRustBuffer[*MoqSubscription](c, rb)
}

func (_ FfiConverterOptionalMoqSubscription) Read(reader io.Reader) *MoqSubscription {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqSubscriptionINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqSubscription) Lower(value *MoqSubscription) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqSubscription](c, value)
}

func (c FfiConverterOptionalMoqSubscription) LowerExternal(value *MoqSubscription) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqSubscription](c, value))
}

func (_ FfiConverterOptionalMoqSubscription) Write(writer io.Writer, value *MoqSubscription) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqSubscriptionINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqSubscription struct{}

func (_ FfiDestroyerOptionalMoqSubscription) Destroy(value *MoqSubscription) {
	if value != nil {
		FfiDestroyerMoqSubscription{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqTrackInfo struct{}

var FfiConverterOptionalMoqTrackInfoINSTANCE = FfiConverterOptionalMoqTrackInfo{}

func (c FfiConverterOptionalMoqTrackInfo) Lift(rb RustBufferI) *MoqTrackInfo {
	return LiftFromRustBuffer[*MoqTrackInfo](c, rb)
}

func (_ FfiConverterOptionalMoqTrackInfo) Read(reader io.Reader) *MoqTrackInfo {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqTrackInfoINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqTrackInfo) Lower(value *MoqTrackInfo) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqTrackInfo](c, value)
}

func (c FfiConverterOptionalMoqTrackInfo) LowerExternal(value *MoqTrackInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqTrackInfo](c, value))
}

func (_ FfiConverterOptionalMoqTrackInfo) Write(writer io.Writer, value *MoqTrackInfo) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqTrackInfoINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqTrackInfo struct{}

func (_ FfiDestroyerOptionalMoqTrackInfo) Destroy(value *MoqTrackInfo) {
	if value != nil {
		FfiDestroyerMoqTrackInfo{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqVideoDecodedFrame struct{}

var FfiConverterOptionalMoqVideoDecodedFrameINSTANCE = FfiConverterOptionalMoqVideoDecodedFrame{}

func (c FfiConverterOptionalMoqVideoDecodedFrame) Lift(rb RustBufferI) *MoqVideoDecodedFrame {
	return LiftFromRustBuffer[*MoqVideoDecodedFrame](c, rb)
}

func (_ FfiConverterOptionalMoqVideoDecodedFrame) Read(reader io.Reader) *MoqVideoDecodedFrame {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqVideoDecodedFrameINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqVideoDecodedFrame) Lower(value *MoqVideoDecodedFrame) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqVideoDecodedFrame](c, value)
}

func (c FfiConverterOptionalMoqVideoDecodedFrame) LowerExternal(value *MoqVideoDecodedFrame) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqVideoDecodedFrame](c, value))
}

func (_ FfiConverterOptionalMoqVideoDecodedFrame) Write(writer io.Writer, value *MoqVideoDecodedFrame) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqVideoDecodedFrameINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqVideoDecodedFrame struct{}

func (_ FfiDestroyerOptionalMoqVideoDecodedFrame) Destroy(value *MoqVideoDecodedFrame) {
	if value != nil {
		FfiDestroyerMoqVideoDecodedFrame{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqVideoHint struct{}

var FfiConverterOptionalMoqVideoHintINSTANCE = FfiConverterOptionalMoqVideoHint{}

func (c FfiConverterOptionalMoqVideoHint) Lift(rb RustBufferI) *MoqVideoHint {
	return LiftFromRustBuffer[*MoqVideoHint](c, rb)
}

func (_ FfiConverterOptionalMoqVideoHint) Read(reader io.Reader) *MoqVideoHint {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqVideoHintINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqVideoHint) Lower(value *MoqVideoHint) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqVideoHint](c, value)
}

func (c FfiConverterOptionalMoqVideoHint) LowerExternal(value *MoqVideoHint) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqVideoHint](c, value))
}

func (_ FfiConverterOptionalMoqVideoHint) Write(writer io.Writer, value *MoqVideoHint) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqVideoHintINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqVideoHint struct{}

func (_ FfiDestroyerOptionalMoqVideoHint) Destroy(value *MoqVideoHint) {
	if value != nil {
		FfiDestroyerMoqVideoHint{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqVideoPixelFormat struct{}

var FfiConverterOptionalMoqVideoPixelFormatINSTANCE = FfiConverterOptionalMoqVideoPixelFormat{}

func (c FfiConverterOptionalMoqVideoPixelFormat) Lift(rb RustBufferI) *MoqVideoPixelFormat {
	return LiftFromRustBuffer[*MoqVideoPixelFormat](c, rb)
}

func (_ FfiConverterOptionalMoqVideoPixelFormat) Read(reader io.Reader) *MoqVideoPixelFormat {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqVideoPixelFormatINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqVideoPixelFormat) Lower(value *MoqVideoPixelFormat) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqVideoPixelFormat](c, value)
}

func (c FfiConverterOptionalMoqVideoPixelFormat) LowerExternal(value *MoqVideoPixelFormat) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqVideoPixelFormat](c, value))
}

func (_ FfiConverterOptionalMoqVideoPixelFormat) Write(writer io.Writer, value *MoqVideoPixelFormat) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqVideoPixelFormatINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqVideoPixelFormat struct{}

func (_ FfiDestroyerOptionalMoqVideoPixelFormat) Destroy(value *MoqVideoPixelFormat) {
	if value != nil {
		FfiDestroyerMoqVideoPixelFormat{}.Destroy(*value)
	}
}

type FfiConverterOptionalSequenceString struct{}

var FfiConverterOptionalSequenceStringINSTANCE = FfiConverterOptionalSequenceString{}

func (c FfiConverterOptionalSequenceString) Lift(rb RustBufferI) *[]string {
	return LiftFromRustBuffer[*[]string](c, rb)
}

func (_ FfiConverterOptionalSequenceString) Read(reader io.Reader) *[]string {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterSequenceStringINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalSequenceString) Lower(value *[]string) C.RustBuffer {
	return LowerIntoRustBuffer[*[]string](c, value)
}

func (c FfiConverterOptionalSequenceString) LowerExternal(value *[]string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*[]string](c, value))
}

func (_ FfiConverterOptionalSequenceString) Write(writer io.Writer, value *[]string) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterSequenceStringINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalSequenceString struct{}

func (_ FfiDestroyerOptionalSequenceString) Destroy(value *[]string) {
	if value != nil {
		FfiDestroyerSequenceString{}.Destroy(*value)
	}
}

type FfiConverterSequenceUint64 struct{}

var FfiConverterSequenceUint64INSTANCE = FfiConverterSequenceUint64{}

func (c FfiConverterSequenceUint64) Lift(rb RustBufferI) []uint64 {
	return LiftFromRustBuffer[[]uint64](c, rb)
}

func (c FfiConverterSequenceUint64) Read(reader io.Reader) []uint64 {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]uint64, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterUint64INSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceUint64) Lower(value []uint64) C.RustBuffer {
	return LowerIntoRustBuffer[[]uint64](c, value)
}

func (c FfiConverterSequenceUint64) LowerExternal(value []uint64) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]uint64](c, value))
}

func (c FfiConverterSequenceUint64) Write(writer io.Writer, value []uint64) {
	if len(value) > math.MaxInt32 {
		panic("[]uint64 is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterUint64INSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceUint64 struct{}

func (FfiDestroyerSequenceUint64) Destroy(sequence []uint64) {
	for _, value := range sequence {
		FfiDestroyerUint64{}.Destroy(value)
	}
}

type FfiConverterSequenceString struct{}

var FfiConverterSequenceStringINSTANCE = FfiConverterSequenceString{}

func (c FfiConverterSequenceString) Lift(rb RustBufferI) []string {
	return LiftFromRustBuffer[[]string](c, rb)
}

func (c FfiConverterSequenceString) Read(reader io.Reader) []string {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]string, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterStringINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceString) Lower(value []string) C.RustBuffer {
	return LowerIntoRustBuffer[[]string](c, value)
}

func (c FfiConverterSequenceString) LowerExternal(value []string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]string](c, value))
}

func (c FfiConverterSequenceString) Write(writer io.Writer, value []string) {
	if len(value) > math.MaxInt32 {
		panic("[]string is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterStringINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceString struct{}

func (FfiDestroyerSequenceString) Destroy(sequence []string) {
	for _, value := range sequence {
		FfiDestroyerString{}.Destroy(value)
	}
}

type FfiConverterMapStringString struct{}

var FfiConverterMapStringStringINSTANCE = FfiConverterMapStringString{}

func (c FfiConverterMapStringString) Lift(rb RustBufferI) map[string]string {
	return LiftFromRustBuffer[map[string]string](c, rb)
}

func (_ FfiConverterMapStringString) Read(reader io.Reader) map[string]string {
	result := make(map[string]string)
	length := readInt32(reader)
	for i := int32(0); i < length; i++ {
		key := FfiConverterStringINSTANCE.Read(reader)
		value := FfiConverterStringINSTANCE.Read(reader)
		result[key] = value
	}
	return result
}

func (c FfiConverterMapStringString) Lower(value map[string]string) C.RustBuffer {
	return LowerIntoRustBuffer[map[string]string](c, value)
}

func (c FfiConverterMapStringString) LowerExternal(value map[string]string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[map[string]string](c, value))
}

func (_ FfiConverterMapStringString) Write(writer io.Writer, mapValue map[string]string) {
	if len(mapValue) > math.MaxInt32 {
		panic("map[string]string is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(mapValue)))
	for key, value := range mapValue {
		FfiConverterStringINSTANCE.Write(writer, key)
		FfiConverterStringINSTANCE.Write(writer, value)
	}
}

type FfiDestroyerMapStringString struct{}

func (_ FfiDestroyerMapStringString) Destroy(mapValue map[string]string) {
	for key, value := range mapValue {
		FfiDestroyerString{}.Destroy(key)
		FfiDestroyerString{}.Destroy(value)
	}
}

type FfiConverterMapStringMoqAudio struct{}

var FfiConverterMapStringMoqAudioINSTANCE = FfiConverterMapStringMoqAudio{}

func (c FfiConverterMapStringMoqAudio) Lift(rb RustBufferI) map[string]MoqAudio {
	return LiftFromRustBuffer[map[string]MoqAudio](c, rb)
}

func (_ FfiConverterMapStringMoqAudio) Read(reader io.Reader) map[string]MoqAudio {
	result := make(map[string]MoqAudio)
	length := readInt32(reader)
	for i := int32(0); i < length; i++ {
		key := FfiConverterStringINSTANCE.Read(reader)
		value := FfiConverterMoqAudioINSTANCE.Read(reader)
		result[key] = value
	}
	return result
}

func (c FfiConverterMapStringMoqAudio) Lower(value map[string]MoqAudio) C.RustBuffer {
	return LowerIntoRustBuffer[map[string]MoqAudio](c, value)
}

func (c FfiConverterMapStringMoqAudio) LowerExternal(value map[string]MoqAudio) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[map[string]MoqAudio](c, value))
}

func (_ FfiConverterMapStringMoqAudio) Write(writer io.Writer, mapValue map[string]MoqAudio) {
	if len(mapValue) > math.MaxInt32 {
		panic("map[string]MoqAudio is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(mapValue)))
	for key, value := range mapValue {
		FfiConverterStringINSTANCE.Write(writer, key)
		FfiConverterMoqAudioINSTANCE.Write(writer, value)
	}
}

type FfiDestroyerMapStringMoqAudio struct{}

func (_ FfiDestroyerMapStringMoqAudio) Destroy(mapValue map[string]MoqAudio) {
	for key, value := range mapValue {
		FfiDestroyerString{}.Destroy(key)
		FfiDestroyerMoqAudio{}.Destroy(value)
	}
}

type FfiConverterMapStringMoqVideo struct{}

var FfiConverterMapStringMoqVideoINSTANCE = FfiConverterMapStringMoqVideo{}

func (c FfiConverterMapStringMoqVideo) Lift(rb RustBufferI) map[string]MoqVideo {
	return LiftFromRustBuffer[map[string]MoqVideo](c, rb)
}

func (_ FfiConverterMapStringMoqVideo) Read(reader io.Reader) map[string]MoqVideo {
	result := make(map[string]MoqVideo)
	length := readInt32(reader)
	for i := int32(0); i < length; i++ {
		key := FfiConverterStringINSTANCE.Read(reader)
		value := FfiConverterMoqVideoINSTANCE.Read(reader)
		result[key] = value
	}
	return result
}

func (c FfiConverterMapStringMoqVideo) Lower(value map[string]MoqVideo) C.RustBuffer {
	return LowerIntoRustBuffer[map[string]MoqVideo](c, value)
}

func (c FfiConverterMapStringMoqVideo) LowerExternal(value map[string]MoqVideo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[map[string]MoqVideo](c, value))
}

func (_ FfiConverterMapStringMoqVideo) Write(writer io.Writer, mapValue map[string]MoqVideo) {
	if len(mapValue) > math.MaxInt32 {
		panic("map[string]MoqVideo is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(mapValue)))
	for key, value := range mapValue {
		FfiConverterStringINSTANCE.Write(writer, key)
		FfiConverterMoqVideoINSTANCE.Write(writer, value)
	}
}

type FfiDestroyerMapStringMoqVideo struct{}

func (_ FfiDestroyerMapStringMoqVideo) Destroy(mapValue map[string]MoqVideo) {
	for key, value := range mapValue {
		FfiDestroyerString{}.Destroy(key)
		FfiDestroyerMoqVideo{}.Destroy(value)
	}
}

const (
	uniffiRustFuturePollReady      int8 = 0
	uniffiRustFuturePollMaybeReady int8 = 1
	uniffiRustCallStatusCancelled  int8 = 3
)

type rustFuturePollFunc func(C.uint64_t, C.UniffiRustFutureContinuationCallback, C.uint64_t)
type rustFutureCompleteFunc[T any] func(C.uint64_t, *C.RustCallStatus) T
type rustFutureFreeFunc func(C.uint64_t)

//export moq_uniffiFutureContinuationCallback
func moq_uniffiFutureContinuationCallback(data C.uint64_t, pollResult C.int8_t) {
	h := cgo.Handle(uintptr(data))
	waiter := h.Value().(chan int8)
	waiter <- int8(pollResult)
}

func uniffiErrorFromRust[E any](err E) error {
	value := reflect.ValueOf(err)
	if !value.IsValid() || value.IsZero() {
		return nil
	}
	if native, ok := any(err).(NativeError); ok {
		return native.AsError()
	}
	if e, ok := any(err).(error); ok {
		return e
	}
	return fmt.Errorf("%v", err)
}

func uniffiCompleteRustFuture[E any, T any, F any](
	errConverter BufReader[E],
	completeFunc rustFutureCompleteFunc[F],
	liftFunc func(F) T,
	rustFuture C.uint64_t,
) (T, error) {
	var goValue T
	var status C.RustCallStatus
	ffiValue := completeFunc(rustFuture, &status)
	switch int8(status.code) {
	case 0:
		return liftFunc(ffiValue), nil
	case uniffiRustCallStatusCancelled:
		return goValue, nil
	default:
		return goValue, uniffiErrorFromRust(checkCallStatus(errConverter, status))
	}
}

func uniffiRustCallAsync[E any, T any, F any](
	ctx context.Context,
	errConverter BufReader[E],
	completeFunc rustFutureCompleteFunc[F],
	liftFunc func(F) T,
	rustFutureFunc func() C.uint64_t,
	pollFunc rustFuturePollFunc,
	cancelFunc rustFutureFreeFunc,
	freeFunc rustFutureFreeFunc,
) (T, error) {
	var goValue T
	if err := ctx.Err(); err != nil {
		return goValue, err
	}

	rustFuture := rustFutureFunc()
	defer freeFunc(rustFuture)

	pollResult := int8(-1)
	waiter := make(chan int8, 1)
	cancelled := false

	chanHandle := cgo.NewHandle(waiter)
	defer chanHandle.Delete()

	for pollResult != uniffiRustFuturePollReady {
		pollFunc(
			rustFuture,
			(C.UniffiRustFutureContinuationCallback)(C.moq_uniffiFutureContinuationCallback),
			C.uint64_t(chanHandle),
		)
		select {
		case pollResult = <-waiter:
		case <-ctx.Done():
			cancelled = true
			cancelFunc(rustFuture)
			pollResult = <-waiter
		}
	}

	result, err := uniffiCompleteRustFuture(errConverter, completeFunc, liftFunc, rustFuture)
	if cancelled {
		if err := ctx.Err(); err != nil {
			return goValue, err
		}
		return goValue, context.Canceled
	}
	if err != nil {
		return goValue, err
	}
	return result, nil
}

//export moq_uniffiFreeGorutine
func moq_uniffiFreeGorutine(data C.uint64_t) {
	handle := cgo.Handle(uintptr(data))
	defer handle.Delete()

	guard := handle.Value().(chan struct{})
	guard <- struct{}{}
}

// Initialize logging with a level string: "error", "warn", "info", "debug", "trace", or "".
//
// Returns an error if called more than once.
func MoqLogLevel(level string) error {
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_func_moq_log_level(FfiConverterStringINSTANCE.Lower(level), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
