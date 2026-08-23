#define WIN32_LEAN_AND_MEAN
#define NOMINMAX

#include <windows.h>
#include <inspectable.h>
#include <roapi.h>
#include <winstring.h>
#include <eventtoken.h>
#include <d3d11.h>
#include <d3d10.h>
#include <dxgi1_6.h>
#include <mfapi.h>
#include <mferror.h>
#include <mfidl.h>
#include <mftransform.h>
#include <codecapi.h>
#include <propvarutil.h>

#include "video_capture_windows.h"

#include <atomic>
#include <cstdint>
#include <new>
#include <vector>

// MinGW has the Media Foundation and Direct3D interfaces used below, but its
// Windows Graphics Capture header still omits these WinRT interfaces. Keep the
// small stable ABI here so the regular MinGW cgo build needs no extra SDK.
struct BorkSizeInt32 {
    INT32 Width;
    INT32 Height;
};

struct BorkTimeSpan {
    INT64 Duration;
};

struct BorkGraphicsCaptureItem;
struct BorkDirect3D11CaptureFrame;
struct BorkDirect3D11CaptureFramePool;
struct BorkGraphicsCaptureSession;

struct BorkFrameArrivedHandler : IUnknown {
    virtual HRESULT STDMETHODCALLTYPE Invoke(
        BorkDirect3D11CaptureFramePool *, IInspectable *) = 0;
};

struct BorkItemClosedHandler : IUnknown {
    virtual HRESULT STDMETHODCALLTYPE Invoke(
        BorkGraphicsCaptureItem *, IInspectable *) = 0;
};

struct BorkGraphicsCaptureItem : IInspectable {
    virtual HRESULT STDMETHODCALLTYPE get_DisplayName(HSTRING *) = 0;
    virtual HRESULT STDMETHODCALLTYPE get_Size(BorkSizeInt32 *) = 0;
    virtual HRESULT STDMETHODCALLTYPE add_Closed(
        BorkItemClosedHandler *, EventRegistrationToken *) = 0;
    virtual HRESULT STDMETHODCALLTYPE remove_Closed(EventRegistrationToken) = 0;
};

struct BorkDirect3D11CaptureFrame : IInspectable {
    virtual HRESULT STDMETHODCALLTYPE get_Surface(IInspectable **) = 0;
    virtual HRESULT STDMETHODCALLTYPE get_SystemRelativeTime(BorkTimeSpan *) = 0;
    virtual HRESULT STDMETHODCALLTYPE get_ContentSize(BorkSizeInt32 *) = 0;
};

struct BorkGraphicsCaptureSession : IInspectable {
    virtual HRESULT STDMETHODCALLTYPE StartCapture() = 0;
};

struct BorkDirect3D11CaptureFramePool : IInspectable {
    virtual HRESULT STDMETHODCALLTYPE Recreate(
        IInspectable *, INT32, INT32, BorkSizeInt32) = 0;
    virtual HRESULT STDMETHODCALLTYPE TryGetNextFrame(
        BorkDirect3D11CaptureFrame **) = 0;
    virtual HRESULT STDMETHODCALLTYPE add_FrameArrived(
        BorkFrameArrivedHandler *, EventRegistrationToken *) = 0;
    virtual HRESULT STDMETHODCALLTYPE remove_FrameArrived(
        EventRegistrationToken) = 0;
    virtual HRESULT STDMETHODCALLTYPE CreateCaptureSession(
        BorkGraphicsCaptureItem *, BorkGraphicsCaptureSession **) = 0;
    virtual HRESULT STDMETHODCALLTYPE get_DispatcherQueue(IInspectable **) = 0;
};

struct BorkFramePoolStatics : IInspectable {
    virtual HRESULT STDMETHODCALLTYPE CreateFreeThreaded(
        IInspectable *, INT32, INT32, BorkSizeInt32,
        BorkDirect3D11CaptureFramePool **) = 0;
};

struct BorkCaptureItemInterop : IUnknown {
    virtual HRESULT STDMETHODCALLTYPE CreateForWindow(
        HWND, REFIID, void **) = 0;
    virtual HRESULT STDMETHODCALLTYPE CreateForMonitor(
        HMONITOR, REFIID, void **) = 0;
};

struct BorkDxgiInterfaceAccess : IUnknown {
    virtual HRESULT STDMETHODCALLTYPE GetInterface(REFIID, void **) = 0;
};

struct BorkClosable : IInspectable {
    virtual HRESULT STDMETHODCALLTYPE Close() = 0;
};

extern "C" HRESULT WINAPI CreateDirect3D11DeviceFromDXGIDevice(
    IDXGIDevice *, IInspectable **);

extern "C" HRESULT WINAPI MFTEnum2(
    GUID, UINT32, const MFT_REGISTER_TYPE_INFO *,
    const MFT_REGISTER_TYPE_INFO *, IMFAttributes *,
    IMFActivate ***, UINT32 *);

static const GUID bork_iid_capture_item = {
    0x79c3f95b, 0x31f7, 0x4ec2, {0xa4, 0x64, 0x63, 0x2e, 0xf5, 0xd3, 0x07, 0x60}};
static const GUID bork_iid_capture_item_interop = {
    0x3628e81b, 0x3cac, 0x4c60, {0xb7, 0xf4, 0x23, 0xce, 0x0e, 0x0c, 0x33, 0x56}};
static const GUID bork_iid_frame_pool_statics = {
    0x589b103f, 0x6bbc, 0x5df5, {0xa9, 0x91, 0x02, 0xe2, 0x8b, 0x3b, 0x66, 0xd5}};
static const GUID bork_iid_frame_handler = {
    0x51a947f7, 0x79cf, 0x5a3e, {0xa3, 0xa5, 0x12, 0x89, 0xcf, 0xa6, 0xdf, 0xe8}};
static const GUID bork_iid_closed_handler = {
    0xe9c610c0, 0xa68c, 0x5bd9, {0x80, 0x21, 0x85, 0x89, 0x34, 0x6e, 0xee, 0xe2}};
static const GUID bork_iid_dxgi_access = {
    0xa9b3d012, 0x3df2, 0x4ee3, {0xb8, 0xd1, 0x86, 0x95, 0xf4, 0x57, 0xd3, 0xc1}};
static const GUID bork_iid_closable = {
    0x30d5a829, 0x7fa4, 0x4026, {0x83, 0xbb, 0xd7, 0x5b, 0xae, 0x4e, 0xa9, 0x9e}};
static const GUID bork_iid_dxgi_output6 = {
    0x068346e8, 0xaaec, 0x4b84, {0xad, 0xd7, 0x13, 0x7f, 0x51, 0x3f, 0x77, 0xa1}};
static const GUID bork_iid_codec_api = {
    0x901db4c7, 0x31ce, 0x41a2, {0x85, 0xdc, 0x8f, 0xa0, 0xbf, 0x41, 0xb8, 0xda}};
static const GUID bork_iid_mf_shutdown = {
    0x97ec2ea4, 0x0e42, 0x4937, {0x97, 0xac, 0x9d, 0x6d, 0x32, 0x88, 0x24, 0xe1}};
static const GUID bork_iid_video_processor_control = {
    0xa3f675d5, 0x6119, 0x4f7f, {0xa1, 0x00, 0x1d, 0x8b, 0x28, 0x0f, 0x0e, 0xfb}};

static const GUID bork_clsid_video_processor = {
    0x88753b26, 0x5b24, 0x49bd, {0xb2, 0xe7, 0x0c, 0x44, 0x5c, 0x78, 0xc9, 0x82}};
static const GUID bork_mf_max_luminance = {
    0x50253128, 0xc110, 0x4de4, {0x98, 0xae, 0x46, 0xa3, 0x24, 0xfa, 0xe6, 0xda}};
static const GUID bork_mft_adapter_luid = {
    0x1d39518c, 0xe220, 0x4da8, {0xa0, 0x7f, 0xba, 0x17, 0x25, 0x52, 0xd6, 0xb1}};
static const GUID bork_xvp_disable_frc = {
    0x2c0afa19, 0x7a97, 0x4d5a, {0x9e, 0xe8, 0x16, 0xd4, 0xfc, 0x51, 0x8d, 0x8c}};

static constexpr INT32 bork_pixel_format_fp16 = 10;
static constexpr UINT32 bork_frame_rate = 15;
static constexpr UINT32 bork_frame_duration_us = 66667;
static constexpr LONGLONG bork_frame_duration_hns = 666667;
static constexpr LONGLONG bork_key_frame_interval_hns = 20000000;
static constexpr UINT32 bork_video_bitrate = 3000000;
static constexpr UINT32 bork_output_luminance_nits = 80;

template <typename T>
static void bork_release(T *&value) {
    if (value != nullptr) {
        value->Release();
        value = nullptr;
    }
}

static void bork_shutdown_transform(IMFTransform *transform) {
    if (transform == nullptr) return;
    IMFShutdown *shutdown = nullptr;
    if (SUCCEEDED(transform->QueryInterface(
            bork_iid_mf_shutdown, reinterpret_cast<void **>(&shutdown)))) {
        shutdown->Shutdown();
    }
    bork_release(shutdown);
}

static HRESULT bork_hresult_from_last_error() {
    DWORD error = GetLastError();
    return HRESULT_FROM_WIN32(error == ERROR_SUCCESS ? ERROR_INVALID_FUNCTION : error);
}

static HRESULT bork_set_common_video_type(
    IMFMediaType *type, REFGUID subtype, UINT32 width, UINT32 height) {
    HRESULT result = type->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
    if (SUCCEEDED(result)) result = type->SetGUID(MF_MT_SUBTYPE, subtype);
    if (SUCCEEDED(result)) result = MFSetAttributeSize(type, MF_MT_FRAME_SIZE, width, height);
    if (SUCCEEDED(result)) result = MFSetAttributeRatio(type, MF_MT_FRAME_RATE, bork_frame_rate, 1);
    if (SUCCEEDED(result)) result = MFSetAttributeRatio(type, MF_MT_PIXEL_ASPECT_RATIO, 1, 1);
    if (SUCCEEDED(result)) result = type->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
    return result;
}

static void bork_close_winrt(IInspectable *value) {
    if (value == nullptr) return;
    BorkClosable *closable = nullptr;
    if (SUCCEEDED(value->QueryInterface(bork_iid_closable, reinterpret_cast<void **>(&closable)))) {
        closable->Close();
        closable->Release();
    }
}

static HRESULT bork_get_activation_factory(
    PCWSTR runtime_class, REFIID iid, void **factory) {
    HSTRING_HEADER header{};
    HSTRING name = nullptr;
    HRESULT result = WindowsCreateStringReference(
        runtime_class, static_cast<UINT32>(lstrlenW(runtime_class)),
        &header, &name);
    if (SUCCEEDED(result)) result = RoGetActivationFactory(name, iid, factory);
    return result;
}

class BorkFrameHandler final : public BorkFrameArrivedHandler {
public:
    explicit BorkFrameHandler(HANDLE event) : event_(event) {}

    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID iid, void **object) override {
        if (object == nullptr) return E_POINTER;
        *object = nullptr;
        if (iid != IID_IUnknown && iid != IID_IAgileObject && iid != bork_iid_frame_handler) {
            return E_NOINTERFACE;
        }
        *object = this;
        AddRef();
        return S_OK;
    }

    ULONG STDMETHODCALLTYPE AddRef() override {
        return refs_.fetch_add(1, std::memory_order_relaxed) + 1;
    }

    ULONG STDMETHODCALLTYPE Release() override {
        ULONG refs = refs_.fetch_sub(1, std::memory_order_acq_rel) - 1;
        if (refs == 0) delete this;
        return refs;
    }

    HRESULT STDMETHODCALLTYPE Invoke(
        BorkDirect3D11CaptureFramePool *, IInspectable *) override {
        SetEvent(event_);
        return S_OK;
    }

private:
    ~BorkFrameHandler() { CloseHandle(event_); }

    std::atomic<ULONG> refs_{1};
    HANDLE event_;
};

class BorkClosedHandler final : public BorkItemClosedHandler {
public:
    explicit BorkClosedHandler(HANDLE event) : event_(event) {}

    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID iid, void **object) override {
        if (object == nullptr) return E_POINTER;
        *object = nullptr;
        if (iid != IID_IUnknown && iid != IID_IAgileObject && iid != bork_iid_closed_handler) {
            return E_NOINTERFACE;
        }
        *object = this;
        AddRef();
        return S_OK;
    }

    ULONG STDMETHODCALLTYPE AddRef() override {
        return refs_.fetch_add(1, std::memory_order_relaxed) + 1;
    }

    ULONG STDMETHODCALLTYPE Release() override {
        ULONG refs = refs_.fetch_sub(1, std::memory_order_acq_rel) - 1;
        if (refs == 0) delete this;
        return refs;
    }

    HRESULT STDMETHODCALLTYPE Invoke(
        BorkGraphicsCaptureItem *, IInspectable *) override {
        SetEvent(event_);
        return S_OK;
    }

private:
    ~BorkClosedHandler() { CloseHandle(event_); }

    std::atomic<ULONG> refs_{1};
    HANDLE event_;
};

struct BorkEncoderEvent {
    MediaEventType type;
    HRESULT status;
};

struct BorkNativeFrame {
    BorkDirect3D11CaptureFrame *frame = nullptr;
    ID3D11Texture2D *texture = nullptr;
    BorkSizeInt32 size{};
    INT64 time_hns = 0;
};

class BorkScreenVideoCapture {
public:
    HRESULT Initialize(
        int32_t source_kind, uintptr_t source_handle,
        uint32_t max_frame_bytes, bork_screen_video_info *info);
    HRESULT Read(bork_screen_video_frame *frame);
    HRESULT ForceKeyFrame();
    HRESULT Stop();
    ~BorkScreenVideoCapture();

private:
    HRESULT InitializeRuntime();
    HRESULT InitializeEvents();
    HRESULT InitializeDevice();
    HRESULT InitializeCaptureItem(int32_t source_kind, uintptr_t source_handle);
    HRESULT InitializeFramePool();
    HRESULT InitializeVideoProcessor();
    HRESULT InitializeEncoder(int32_t *codec);
    HRESULT StartTransforms();
    HRESULT CreateCaptureItem(
        BorkCaptureItemInterop *interop, int32_t kind, uintptr_t handle);
    HRESULT ConfigureVideoProcessorInput(BorkSizeInt32 size);
    HRESULT ConfigureVideoProcessorGeometry(BorkSizeInt32 size);
    HRESULT CreateProcessorType(
        REFGUID subtype, BorkSizeInt32 size, bool input,
        UINT32 max_luminance, IMFMediaType **type);

    HRESULT CreateHardwareEncoder(IMFTransform **encoder);
    HRESULT CreateEncoderTypes(UINT32 profile, IMFMediaType **input, IMFMediaType **output);
    HRESULT ConfigureEncoderTypes(int32_t *codec);
    HRESULT ConfigureEncoderCodec();
    HRESULT ConfigureEncoderOutputSample();

    HRESULT ReadEncoderEvent(BorkEncoderEvent *event);
    HRESULT RecordEncoderEvent(const BorkEncoderEvent &event);
    HRESULT PumpEncoderEvents();
    HRESULT WaitForProgress();
    HRESULT PollFrame(BorkNativeFrame *frame, bool *found);
    HRESULT TakeReadyFrame(BorkNativeFrame *frame, bool *found);
    HRESULT TakeLatestFrame(BorkNativeFrame *frame);
    HRESULT OpenFrame(BorkDirect3D11CaptureFrame *frame, BorkNativeFrame *value);
    void ReleaseFrame(BorkNativeFrame *frame);
    HRESULT RefreshVideoInput(BorkSizeInt32 size, bool recreate_frame_pool);
    bool VideoInputChanged(const BorkNativeFrame *frame, bool *resized) const;
    bool ShouldEncode(INT64 time_hns);

    HRESULT PumpOnce(bork_screen_video_frame *output, bool *produced);
    HRESULT SubmitFrame(BorkNativeFrame *frame);
    HRESULT MakeInputSample(BorkNativeFrame *frame, IMFSample **sample);
    HRESULT ProcessVideoProcessor(IMFSample *input, IMFSample **output);
    HRESULT SubmitEncoderInput(IMFSample *input);
    HRESULT TakeEncoderOutput(bork_screen_video_frame *output);
    HRESULT RequestEncoderKeyFrame(IMFSample *input);
    HRESULT MakeEncoderOutputBuffer(MFT_OUTPUT_DATA_BUFFER *output);
    HRESULT ReadEncoderOutput(IMFSample *sample, bork_screen_video_frame *output);
    HRESULT CopyEncodedBytes(IMFSample *sample, bool *key_frame);
    HRESULT RefreshSequenceHeader();

    HMONITOR CurrentMonitor() const;
    UINT32 MonitorLuminance(HMONITOR monitor) const;
    static void FitOutput(BorkSizeInt32 input, UINT32 *width, UINT32 *height);
    RECT AspectFitRectangle(BorkSizeInt32 input) const;

    bool ro_initialized_ = false;
    bool mf_started_ = false;
    bool transforms_started_ = false;

    HANDLE stop_event_ = nullptr;
    HANDLE frame_event_ = nullptr;
    HANDLE closed_event_ = nullptr;
    BorkFrameHandler *frame_handler_ = nullptr;
    BorkClosedHandler *closed_handler_ = nullptr;

    ID3D11Device *device_ = nullptr;
    IMFDXGIDeviceManager *device_manager_ = nullptr;
    UINT device_manager_token_ = 0;
    IInspectable *winrt_device_ = nullptr;

    BorkGraphicsCaptureItem *capture_item_ = nullptr;
    BorkDirect3D11CaptureFramePool *frame_pool_ = nullptr;
    BorkGraphicsCaptureSession *capture_session_ = nullptr;
    EventRegistrationToken frame_token_{};
    EventRegistrationToken closed_token_{};
    BorkSizeInt32 input_size_{};
    HMONITOR input_monitor_ = nullptr;
    int32_t source_kind_ = BORK_SCREEN_VIDEO_SOURCE_MONITOR;
    uintptr_t source_handle_ = 0;

    IMFTransform *video_processor_ = nullptr;
    IMFVideoProcessorControl *video_processor_control_ = nullptr;
    IMFTransform *encoder_ = nullptr;
    IMFMediaEventGenerator *encoder_events_ = nullptr;
    ICodecAPI *codec_api_ = nullptr;
    MFT_OUTPUT_STREAM_INFO encoder_output_info_{};
    IMFSample *encoder_output_sample_ = nullptr;
    UINT32 encoder_need_input_ = 0;
    UINT32 encoder_have_output_ = 0;

    UINT32 output_width_ = 0;
    UINT32 output_height_ = 0;
    UINT32 max_frame_bytes_ = 0;
    std::atomic<bool> force_key_frame_{true};
    INT64 next_key_frame_hns_ = 0;
    INT64 first_time_hns_ = -1;
    INT64 last_time_hns_ = -1;
    std::vector<uint8_t> sequence_header_;
    std::vector<uint8_t> encoded_frame_;
};

HRESULT BorkScreenVideoCapture::InitializeRuntime() {
    HRESULT result = RoInitialize(RO_INIT_MULTITHREADED);
    if (result != RPC_E_CHANGED_MODE && FAILED(result)) return result;
    ro_initialized_ = result != RPC_E_CHANGED_MODE;
    result = MFStartup(MF_VERSION, MFSTARTUP_LITE);
    if (SUCCEEDED(result)) mf_started_ = true;
    return result;
}

HRESULT BorkScreenVideoCapture::InitializeEvents() {
    stop_event_ = CreateEventW(nullptr, TRUE, FALSE, nullptr);
    frame_event_ = CreateEventW(nullptr, FALSE, FALSE, nullptr);
    closed_event_ = CreateEventW(nullptr, TRUE, FALSE, nullptr);
    if (stop_event_ == nullptr || frame_event_ == nullptr ||
        closed_event_ == nullptr) {
        return bork_hresult_from_last_error();
    }
    frame_handler_ = new (std::nothrow) BorkFrameHandler(frame_event_);
    closed_handler_ = new (std::nothrow) BorkClosedHandler(closed_event_);
    if (frame_handler_ == nullptr || closed_handler_ == nullptr) return E_OUTOFMEMORY;
    return S_OK;
}

HRESULT BorkScreenVideoCapture::InitializeDevice() {
    const D3D_FEATURE_LEVEL levels[] = {
        D3D_FEATURE_LEVEL_11_1,
        D3D_FEATURE_LEVEL_11_0,
    };
    D3D_FEATURE_LEVEL level{};
    UINT flags = D3D11_CREATE_DEVICE_BGRA_SUPPORT | D3D11_CREATE_DEVICE_VIDEO_SUPPORT;
    HRESULT result = D3D11CreateDevice(
        nullptr, D3D_DRIVER_TYPE_HARDWARE, nullptr, flags,
        levels, ARRAYSIZE(levels), D3D11_SDK_VERSION,
        &device_, &level, nullptr);
    if (FAILED(result)) return result;

    ID3D10Multithread *multithread = nullptr;
    result = device_->QueryInterface(IID_ID3D10Multithread,
        reinterpret_cast<void **>(&multithread));
    if (SUCCEEDED(result)) multithread->SetMultithreadProtected(TRUE);
    bork_release(multithread);
    if (FAILED(result)) return result;

    result = MFCreateDXGIDeviceManager(&device_manager_token_, &device_manager_);
    if (SUCCEEDED(result)) {
        result = device_manager_->ResetDevice(device_, device_manager_token_);
    }
    if (FAILED(result)) return result;

    IDXGIDevice *dxgi_device = nullptr;
    result = device_->QueryInterface(IID_IDXGIDevice,
        reinterpret_cast<void **>(&dxgi_device));
    if (SUCCEEDED(result)) {
        result = CreateDirect3D11DeviceFromDXGIDevice(dxgi_device, &winrt_device_);
    }
    bork_release(dxgi_device);
    return result;
}

HRESULT BorkScreenVideoCapture::CreateCaptureItem(
    BorkCaptureItemInterop *interop, int32_t kind, uintptr_t handle) {
    if (kind == BORK_SCREEN_VIDEO_SOURCE_MONITOR) {
        return interop->CreateForMonitor(
            reinterpret_cast<HMONITOR>(handle), bork_iid_capture_item,
            reinterpret_cast<void **>(&capture_item_));
    }
    if (kind == BORK_SCREEN_VIDEO_SOURCE_WINDOW) {
        return interop->CreateForWindow(
            reinterpret_cast<HWND>(handle), bork_iid_capture_item,
            reinterpret_cast<void **>(&capture_item_));
    }
    return E_INVALIDARG;
}

HRESULT BorkScreenVideoCapture::InitializeCaptureItem(
    int32_t source_kind, uintptr_t source_handle) {
    BorkCaptureItemInterop *interop = nullptr;
    HRESULT result = bork_get_activation_factory(
        L"Windows.Graphics.Capture.GraphicsCaptureItem",
        bork_iid_capture_item_interop,
        reinterpret_cast<void **>(&interop));
    if (SUCCEEDED(result)) {
        result = CreateCaptureItem(interop, source_kind, source_handle);
    }
    bork_release(interop);
    if (FAILED(result)) return result;

    result = capture_item_->get_Size(&input_size_);
    if (FAILED(result)) return result;
    if (input_size_.Width < 2 || input_size_.Height < 2) return E_INVALIDARG;
    source_kind_ = source_kind;
    source_handle_ = source_handle;
    FitOutput(input_size_, &output_width_, &output_height_);
    return S_OK;
}

HRESULT BorkScreenVideoCapture::InitializeFramePool() {
    BorkFramePoolStatics *statics = nullptr;
    HRESULT result = bork_get_activation_factory(
        L"Windows.Graphics.Capture.Direct3D11CaptureFramePool",
        bork_iid_frame_pool_statics,
        reinterpret_cast<void **>(&statics));
    if (SUCCEEDED(result)) {
        result = statics->CreateFreeThreaded(
            winrt_device_, bork_pixel_format_fp16, 2,
            input_size_, &frame_pool_);
    }
    bork_release(statics);
    if (FAILED(result)) return result;

    result = frame_pool_->CreateCaptureSession(capture_item_, &capture_session_);
    if (SUCCEEDED(result)) {
        result = frame_pool_->add_FrameArrived(frame_handler_, &frame_token_);
    }
    if (SUCCEEDED(result)) {
        result = capture_item_->add_Closed(closed_handler_, &closed_token_);
    }
    return result;
}

HRESULT BorkScreenVideoCapture::CreateProcessorType(
    REFGUID subtype, BorkSizeInt32 size, bool input,
    UINT32 max_luminance, IMFMediaType **type) {
    IMFMediaType *value = nullptr;
    HRESULT result = MFCreateMediaType(&value);
    if (SUCCEEDED(result)) {
        result = bork_set_common_video_type(
            value, subtype, static_cast<UINT32>(size.Width),
            static_cast<UINT32>(size.Height));
    }
    if (SUCCEEDED(result)) result = value->SetUINT32(
        MF_MT_VIDEO_PRIMARIES, MFVideoPrimaries_BT709);
    if (SUCCEEDED(result)) result = value->SetUINT32(
        MF_MT_TRANSFER_FUNCTION, input ? MFVideoTransFunc_10 : MFVideoTransFunc_709);
    if (SUCCEEDED(result)) result = value->SetUINT32(
        MF_MT_VIDEO_NOMINAL_RANGE, input ? MFNominalRange_0_255 : MFNominalRange_16_235);
    if (SUCCEEDED(result)) result = value->SetUINT32(
        bork_mf_max_luminance, max_luminance);
    if (FAILED(result)) {
        bork_release(value);
        return result;
    }
    *type = value;
    return S_OK;
}

HRESULT BorkScreenVideoCapture::ConfigureVideoProcessorInput(BorkSizeInt32 size) {
    HMONITOR monitor = CurrentMonitor();
    UINT32 luminance = monitor == nullptr
        ? bork_output_luminance_nits : MonitorLuminance(monitor);
    IMFMediaType *type = nullptr;
    HRESULT result = CreateProcessorType(
        MFVideoFormat_A16B16G16R16F, size, true, luminance, &type);
    if (SUCCEEDED(result) && transforms_started_) {
        result = video_processor_->ProcessMessage(MFT_MESSAGE_COMMAND_FLUSH, 0);
    }
    if (SUCCEEDED(result)) result = video_processor_->SetInputType(0, type, 0);
    bork_release(type);
    if (SUCCEEDED(result)) result = ConfigureVideoProcessorGeometry(size);
    if (SUCCEEDED(result) && transforms_started_) {
        result = video_processor_->ProcessMessage(
            MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);
    }
    if (SUCCEEDED(result)) input_monitor_ = monitor;
    return result;
}

HRESULT BorkScreenVideoCapture::InitializeVideoProcessor() {
    HRESULT result = CoCreateInstance(
        bork_clsid_video_processor, nullptr, CLSCTX_INPROC_SERVER,
        IID_IMFTransform, reinterpret_cast<void **>(&video_processor_));
    if (FAILED(result)) return result;

    result = video_processor_->QueryInterface(
        bork_iid_video_processor_control,
        reinterpret_cast<void **>(&video_processor_control_));
    if (FAILED(result)) return result;

    IMFAttributes *attributes = nullptr;
    result = video_processor_->GetAttributes(&attributes);
    if (SUCCEEDED(result)) result = attributes->SetUINT32(bork_xvp_disable_frc, TRUE);
    if (SUCCEEDED(result)) result = attributes->SetUINT32(MF_LOW_LATENCY, TRUE);
    bork_release(attributes);
    if (FAILED(result)) return result;

    result = video_processor_->ProcessMessage(
        MFT_MESSAGE_SET_D3D_MANAGER,
        reinterpret_cast<ULONG_PTR>(device_manager_));
    if (FAILED(result)) return result;

    BorkSizeInt32 output_size{
        static_cast<INT32>(output_width_),
        static_cast<INT32>(output_height_)};
    IMFMediaType *output = nullptr;
    result = CreateProcessorType(
        MFVideoFormat_NV12, output_size, false,
        bork_output_luminance_nits, &output);
    if (SUCCEEDED(result)) result = output->SetUINT32(
        MF_MT_YUV_MATRIX, MFVideoTransferMatrix_BT709);
    if (SUCCEEDED(result)) result = output->SetUINT32(
        MF_MT_VIDEO_CHROMA_SITING, MFVideoChromaSubsampling_MPEG2);
    if (SUCCEEDED(result)) result = output->SetUINT32(
        MF_SA_D3D11_BINDFLAGS, D3D11_BIND_VIDEO_ENCODER);
    if (SUCCEEDED(result)) result = video_processor_->SetOutputType(0, output, 0);
    bork_release(output);
    if (FAILED(result)) return result;
    return ConfigureVideoProcessorInput(input_size_);
}

void BorkScreenVideoCapture::FitOutput(
    BorkSizeInt32 input, UINT32 *width, UINT32 *height) {
    UINT32 source_width = static_cast<UINT32>(input.Width);
    UINT32 source_height = static_cast<UINT32>(input.Height);
    UINT64 scaled_width = static_cast<UINT64>(source_width) * 720;
    UINT64 scaled_height = static_cast<UINT64>(source_height) * 1280;
    if (source_width > 1280 || source_height > 720) {
        if (scaled_width <= scaled_height) {
            source_width = static_cast<UINT32>(scaled_width / source_height);
            source_height = 720;
        } else {
            source_height = static_cast<UINT32>(scaled_height / source_width);
            source_width = 1280;
        }
    }
    *width = source_width & ~1U;
    *height = source_height & ~1U;
    if (*width < 2) *width = 2;
    if (*height < 2) *height = 2;
}

RECT BorkScreenVideoCapture::AspectFitRectangle(BorkSizeInt32 input) const {
    UINT32 width = output_width_;
    UINT32 height = output_height_;
    UINT64 input_width = static_cast<UINT32>(input.Width);
    UINT64 input_height = static_cast<UINT32>(input.Height);
    if (input_width * output_height_ > static_cast<UINT64>(output_width_) * input_height) {
        height = static_cast<UINT32>(input_height * output_width_ / input_width);
    } else {
        width = static_cast<UINT32>(input_width * output_height_ / input_height);
    }
    width &= ~1U;
    height &= ~1U;
    if (width < 2) width = 2;
    if (height < 2) height = 2;
    LONG left = static_cast<LONG>(((output_width_ - width) / 2) & ~1U);
    LONG top = static_cast<LONG>(((output_height_ - height) / 2) & ~1U);
    return {left, top, left + static_cast<LONG>(width), top + static_cast<LONG>(height)};
}

HRESULT BorkScreenVideoCapture::ConfigureVideoProcessorGeometry(BorkSizeInt32 size) {
    // XVP otherwise stretches every input to the fixed encoder size. Keep the
    // source ratio and let it fill the remaining area with opaque black.
    MFARGB black{0, 0, 0, 255};
    HRESULT result = video_processor_control_->SetBorderColor(&black);
    RECT destination = AspectFitRectangle(size);
    if (SUCCEEDED(result)) {
        result = video_processor_control_->SetDestinationRectangle(&destination);
    }
    return result;
}

static HRESULT bork_device_adapter_luid(ID3D11Device *device, LUID *luid) {
    IDXGIDevice *dxgi_device = nullptr;
    HRESULT result = device->QueryInterface(
        IID_IDXGIDevice, reinterpret_cast<void **>(&dxgi_device));
    IDXGIAdapter *adapter = nullptr;
    if (SUCCEEDED(result)) result = dxgi_device->GetAdapter(&adapter);
    DXGI_ADAPTER_DESC description{};
    if (SUCCEEDED(result)) result = adapter->GetDesc(&description);
    if (SUCCEEDED(result)) *luid = description.AdapterLuid;
    bork_release(adapter);
    bork_release(dxgi_device);
    return result;
}

static void bork_release_activations(IMFActivate **values, UINT32 count) {
    if (values == nullptr) return;
    for (UINT32 index = 0; index < count; ++index) {
        bork_release(values[index]);
    }
    CoTaskMemFree(values);
}

HRESULT BorkScreenVideoCapture::CreateHardwareEncoder(IMFTransform **encoder) {
    LUID adapter_luid{};
    HRESULT result = bork_device_adapter_luid(device_, &adapter_luid);
    if (FAILED(result)) return result;

    IMFAttributes *attributes = nullptr;
    result = MFCreateAttributes(&attributes, 1);
    if (SUCCEEDED(result)) {
        result = attributes->SetBlob(
            bork_mft_adapter_luid,
            reinterpret_cast<const UINT8 *>(&adapter_luid),
            sizeof(adapter_luid));
    }

    MFT_REGISTER_TYPE_INFO input{MFMediaType_Video, MFVideoFormat_NV12};
    MFT_REGISTER_TYPE_INFO output{MFMediaType_Video, MFVideoFormat_H264};
    IMFActivate **activations = nullptr;
    UINT32 count = 0;
    UINT32 flags = MFT_ENUM_FLAG_HARDWARE |
        MFT_ENUM_FLAG_ASYNCMFT | MFT_ENUM_FLAG_SORTANDFILTER;
    if (SUCCEEDED(result)) {
        result = MFTEnum2(
            MFT_CATEGORY_VIDEO_ENCODER, flags,
            &input, &output, attributes, &activations, &count);
    }
    bork_release(attributes);
    if (SUCCEEDED(result) && count == 0) result = MF_E_TOPO_CODEC_NOT_FOUND;
    if (SUCCEEDED(result)) {
        result = activations[0]->ActivateObject(
            IID_IMFTransform, reinterpret_cast<void **>(encoder));
    }
    bork_release_activations(activations, count);
    return result;
}

HRESULT BorkScreenVideoCapture::CreateEncoderTypes(
    UINT32 profile, IMFMediaType **input, IMFMediaType **output) {
    IMFMediaType *input_type = nullptr;
    HRESULT result = MFCreateMediaType(&input_type);
    if (SUCCEEDED(result)) {
        result = bork_set_common_video_type(
            input_type, MFVideoFormat_NV12, output_width_, output_height_);
    }
    if (SUCCEEDED(result)) result = input_type->SetUINT32(
        MF_MT_VIDEO_PRIMARIES, MFVideoPrimaries_BT709);
    if (SUCCEEDED(result)) result = input_type->SetUINT32(
        MF_MT_TRANSFER_FUNCTION, MFVideoTransFunc_709);
    if (SUCCEEDED(result)) result = input_type->SetUINT32(
        MF_MT_YUV_MATRIX, MFVideoTransferMatrix_BT709);
    if (SUCCEEDED(result)) result = input_type->SetUINT32(
        MF_MT_VIDEO_NOMINAL_RANGE, MFNominalRange_16_235);
    if (FAILED(result)) {
        bork_release(input_type);
        return result;
    }

    IMFMediaType *output_type = nullptr;
    result = MFCreateMediaType(&output_type);
    if (SUCCEEDED(result)) {
        result = bork_set_common_video_type(
            output_type, MFVideoFormat_H264, output_width_, output_height_);
    }
    if (SUCCEEDED(result)) result = output_type->SetUINT32(MF_MT_AVG_BITRATE, bork_video_bitrate);
    if (SUCCEEDED(result)) result = output_type->SetUINT32(MF_MT_MPEG2_PROFILE, profile);
    if (SUCCEEDED(result)) result = output_type->SetUINT32(MF_MT_MPEG2_LEVEL, eAVEncH264VLevel3_1);
    if (FAILED(result)) {
        bork_release(input_type);
        bork_release(output_type);
        return result;
    }
    *input = input_type;
    *output = output_type;
    return S_OK;
}

HRESULT BorkScreenVideoCapture::ConfigureEncoderTypes(int32_t *codec) {
    const UINT32 profiles[] = {
        eAVEncH264VProfile_Main,
        eAVEncH264VProfile_Base,
    };
    IMFMediaType *input = nullptr;
    IMFMediaType *output = nullptr;
    HRESULT result = E_FAIL;
    UINT32 selected = 0;
    for (UINT32 profile : profiles) {
        result = CreateEncoderTypes(profile, &input, &output);
        if (SUCCEEDED(result)) {
            result = encoder_->SetOutputType(0, output, MFT_SET_TYPE_TEST_ONLY);
        }
        if (SUCCEEDED(result)) {
            selected = profile;
            break;
        }
        bork_release(input);
        bork_release(output);
    }
    if (SUCCEEDED(result)) result = encoder_->SetOutputType(0, output, 0);
    if (SUCCEEDED(result)) result = encoder_->SetInputType(0, input, 0);
    bork_release(input);
    bork_release(output);
    if (SUCCEEDED(result)) {
        *codec = selected == eAVEncH264VProfile_Main
            ? BORK_SCREEN_VIDEO_CODEC_H264_MAIN
            : BORK_SCREEN_VIDEO_CODEC_H264_BASELINE;
    }
    return result;
}

static void bork_try_set_codec_uint32(ICodecAPI *codec, const GUID &key, UINT32 value) {
    VARIANT setting;
    VariantInit(&setting);
    setting.vt = VT_UI4;
    setting.ulVal = value;
    codec->SetValue(&key, &setting);
}

static void bork_try_set_codec_bool(ICodecAPI *codec, const GUID &key, bool value) {
    VARIANT setting;
    VariantInit(&setting);
    setting.vt = VT_BOOL;
    setting.boolVal = value ? VARIANT_TRUE : VARIANT_FALSE;
    codec->SetValue(&key, &setting);
}

HRESULT BorkScreenVideoCapture::ConfigureEncoderCodec() {
    HRESULT result = encoder_->QueryInterface(
        bork_iid_codec_api, reinterpret_cast<void **>(&codec_api_));
    if (FAILED(result)) return result;

    // Hardware vendors expose different optional controls. The media type and
    // MF_LOW_LATENCY are authoritative; these standard controls improve the
    // result where the selected driver supports them.
    bork_try_set_codec_bool(codec_api_, CODECAPI_AVLowLatencyMode, true);
    bork_try_set_codec_uint32(
        codec_api_, CODECAPI_AVEncCommonRateControlMode,
        eAVEncCommonRateControlMode_CBR);
    bork_try_set_codec_uint32(
        codec_api_, CODECAPI_AVEncCommonMeanBitRate, bork_video_bitrate);
    bork_try_set_codec_uint32(
        codec_api_, CODECAPI_AVEncMPVGOPSize, bork_frame_rate * 2);
    bork_try_set_codec_uint32(
        codec_api_, CODECAPI_AVEncMPVDefaultBPictureCount, 0);
    return S_OK;
}

HRESULT BorkScreenVideoCapture::ConfigureEncoderOutputSample() {
    HRESULT result = encoder_->GetOutputStreamInfo(0, &encoder_output_info_);
    if (FAILED(result)) return result;
    DWORD supplied = MFT_OUTPUT_STREAM_PROVIDES_SAMPLES |
        MFT_OUTPUT_STREAM_CAN_PROVIDE_SAMPLES;
    if ((encoder_output_info_.dwFlags & supplied) != 0) return S_OK;

    UINT32 capacity = encoder_output_info_.cbSize;
    if (capacity < max_frame_bytes_) capacity = max_frame_bytes_;
    // MFCreateAlignedMemoryBuffer takes the alignment mask, not the byte count.
    UINT32 alignment = encoder_output_info_.cbAlignment > 1
        ? encoder_output_info_.cbAlignment - 1
        : MF_1_BYTE_ALIGNMENT;
    IMFMediaBuffer *buffer = nullptr;
    result = MFCreateSample(&encoder_output_sample_);
    if (SUCCEEDED(result)) {
        result = MFCreateAlignedMemoryBuffer(capacity, alignment, &buffer);
    }
    if (SUCCEEDED(result)) result = encoder_output_sample_->AddBuffer(buffer);
    bork_release(buffer);
    return result;
}

HRESULT BorkScreenVideoCapture::InitializeEncoder(int32_t *codec) {
    HRESULT result = CreateHardwareEncoder(&encoder_);
    if (FAILED(result)) return result;

    IMFAttributes *attributes = nullptr;
    result = encoder_->GetAttributes(&attributes);
    UINT32 asynchronous = FALSE;
    if (SUCCEEDED(result)) result = attributes->GetUINT32(MF_TRANSFORM_ASYNC, &asynchronous);
    if (SUCCEEDED(result) && asynchronous == FALSE) result = MF_E_TOPO_CODEC_NOT_FOUND;
    if (SUCCEEDED(result)) result = attributes->SetUINT32(MF_TRANSFORM_ASYNC_UNLOCK, TRUE);
    if (SUCCEEDED(result)) result = attributes->SetUINT32(MF_LOW_LATENCY, TRUE);
    bork_release(attributes);
    if (FAILED(result)) return result;

    result = encoder_->ProcessMessage(
        MFT_MESSAGE_SET_D3D_MANAGER,
        reinterpret_cast<ULONG_PTR>(device_manager_));
    // Static CodecAPI settings take effect when the output media type is set.
    if (SUCCEEDED(result)) result = ConfigureEncoderCodec();
    if (SUCCEEDED(result)) result = ConfigureEncoderTypes(codec);
    if (SUCCEEDED(result)) result = ConfigureEncoderOutputSample();
    if (SUCCEEDED(result)) {
        result = encoder_->QueryInterface(
            IID_IMFMediaEventGenerator,
            reinterpret_cast<void **>(&encoder_events_));
    }
    return result;
}

HRESULT BorkScreenVideoCapture::StartTransforms() {
    HRESULT result = video_processor_->ProcessMessage(
        MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
    if (SUCCEEDED(result)) {
        result = video_processor_->ProcessMessage(
            MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);
    }
    if (SUCCEEDED(result)) {
        result = encoder_->ProcessMessage(MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
    }
    if (SUCCEEDED(result)) {
        result = encoder_->ProcessMessage(MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);
    }
    if (SUCCEEDED(result)) transforms_started_ = true;
    return result;
}

HRESULT BorkScreenVideoCapture::Initialize(
    int32_t source_kind, uintptr_t source_handle,
    uint32_t max_frame_bytes, bork_screen_video_info *info) {
    if (info == nullptr || max_frame_bytes == 0) return E_INVALIDARG;
    max_frame_bytes_ = max_frame_bytes;

    HRESULT result = InitializeRuntime();
    if (SUCCEEDED(result)) result = InitializeEvents();
    if (SUCCEEDED(result)) result = InitializeDevice();
    if (SUCCEEDED(result)) result = InitializeCaptureItem(source_kind, source_handle);
    if (SUCCEEDED(result)) result = InitializeFramePool();
    if (SUCCEEDED(result)) result = InitializeVideoProcessor();
    int32_t codec = BORK_SCREEN_VIDEO_CODEC_H264_BASELINE;
    if (SUCCEEDED(result)) result = InitializeEncoder(&codec);
    if (SUCCEEDED(result)) result = StartTransforms();
    if (SUCCEEDED(result)) result = capture_session_->StartCapture();
    if (FAILED(result)) return result;

    info->width = output_width_;
    info->height = output_height_;
    info->codec = codec;
    return S_OK;
}

static UINT32 bork_output_luminance(IDXGIOutput *output, HMONITOR monitor) {
    DXGI_OUTPUT_DESC basic{};
    if (FAILED(output->GetDesc(&basic)) || basic.Monitor != monitor) return 0;

    IDXGIOutput6 *output6 = nullptr;
    HRESULT result = output->QueryInterface(
        bork_iid_dxgi_output6, reinterpret_cast<void **>(&output6));
    DXGI_OUTPUT_DESC1 description{};
    if (SUCCEEDED(result)) result = output6->GetDesc1(&description);
    bork_release(output6);
    if (FAILED(result)) return bork_output_luminance_nits;
    if (description.ColorSpace != DXGI_COLOR_SPACE_RGB_FULL_G2084_NONE_P2020) {
        return bork_output_luminance_nits;
    }
    if (description.MaxLuminance < bork_output_luminance_nits) return 1000;
    return static_cast<UINT32>(description.MaxLuminance + 0.5f);
}

HMONITOR BorkScreenVideoCapture::CurrentMonitor() const {
    if (source_kind_ == BORK_SCREEN_VIDEO_SOURCE_MONITOR) {
        return reinterpret_cast<HMONITOR>(source_handle_);
    }
    return MonitorFromWindow(
        reinterpret_cast<HWND>(source_handle_), MONITOR_DEFAULTTONEAREST);
}

UINT32 BorkScreenVideoCapture::MonitorLuminance(HMONITOR monitor) const {
    IDXGIFactory1 *factory = nullptr;
    HRESULT result = CreateDXGIFactory1(
        IID_IDXGIFactory1, reinterpret_cast<void **>(&factory));
    if (FAILED(result)) return bork_output_luminance_nits;

    UINT32 luminance = 0;
    for (UINT adapter_index = 0; luminance == 0; ++adapter_index) {
        IDXGIAdapter1 *adapter = nullptr;
        if (factory->EnumAdapters1(adapter_index, &adapter) == DXGI_ERROR_NOT_FOUND) break;
        for (UINT output_index = 0; luminance == 0; ++output_index) {
            IDXGIOutput *output = nullptr;
            HRESULT next = adapter->EnumOutputs(output_index, &output);
            if (next == DXGI_ERROR_NOT_FOUND) break;
            if (SUCCEEDED(next)) luminance = bork_output_luminance(output, monitor);
            bork_release(output);
        }
        bork_release(adapter);
    }
    bork_release(factory);
    return luminance == 0 ? bork_output_luminance_nits : luminance;
}

void BorkScreenVideoCapture::ReleaseFrame(BorkNativeFrame *frame) {
    bork_release(frame->texture);
    bork_release(frame->frame);
    frame->size = {};
    frame->time_hns = 0;
}

HRESULT BorkScreenVideoCapture::OpenFrame(
    BorkDirect3D11CaptureFrame *frame, BorkNativeFrame *value) {
    value->frame = frame;
    HRESULT result = frame->get_ContentSize(&value->size);
    BorkTimeSpan time{};
    if (SUCCEEDED(result)) result = frame->get_SystemRelativeTime(&time);
    value->time_hns = time.Duration;

    IInspectable *surface = nullptr;
    if (SUCCEEDED(result)) result = frame->get_Surface(&surface);
    BorkDxgiInterfaceAccess *access = nullptr;
    if (SUCCEEDED(result)) {
        result = surface->QueryInterface(
            bork_iid_dxgi_access, reinterpret_cast<void **>(&access));
    }
    if (SUCCEEDED(result)) {
        result = access->GetInterface(
            IID_ID3D11Texture2D, reinterpret_cast<void **>(&value->texture));
    }
    bork_release(access);
    bork_release(surface);
    if (FAILED(result)) return result;

    D3D11_TEXTURE2D_DESC description{};
    value->texture->GetDesc(&description);
    if (description.Format != DXGI_FORMAT_R16G16B16A16_FLOAT) return E_UNEXPECTED;
    return S_OK;
}

HRESULT BorkScreenVideoCapture::TakeLatestFrame(BorkNativeFrame *value) {
    BorkDirect3D11CaptureFrame *latest = nullptr;
    for (;;) {
        BorkDirect3D11CaptureFrame *next = nullptr;
        HRESULT result = frame_pool_->TryGetNextFrame(&next);
        if (FAILED(result)) {
            bork_release(latest);
            return result;
        }
        if (next == nullptr) break;
        bork_release(latest);
        latest = next;
    }
    if (latest == nullptr) return S_FALSE;
    HRESULT result = OpenFrame(latest, value);
    if (FAILED(result)) ReleaseFrame(value);
    return result;
}

HRESULT BorkScreenVideoCapture::RefreshVideoInput(
    BorkSizeInt32 size, bool recreate_frame_pool) {
    if (size.Width < 2 || size.Height < 2) return S_FALSE;
    HRESULT result = S_OK;
    if (recreate_frame_pool) {
        result = frame_pool_->Recreate(
            winrt_device_, bork_pixel_format_fp16, 2, size);
    }
    if (SUCCEEDED(result)) result = ConfigureVideoProcessorInput(size);
    if (SUCCEEDED(result)) {
        input_size_ = size;
        force_key_frame_.store(true, std::memory_order_release);
    }
    return result;
}

bool BorkScreenVideoCapture::ShouldEncode(INT64 time_hns) {
    if (first_time_hns_ < 0) {
        first_time_hns_ = time_hns;
        last_time_hns_ = time_hns;
        return true;
    }
    if (time_hns < last_time_hns_ + bork_frame_duration_hns) return false;
    last_time_hns_ = time_hns;
    return true;
}

bool BorkScreenVideoCapture::VideoInputChanged(
    const BorkNativeFrame *frame, bool *resized) const {
    *resized = frame->size.Width != input_size_.Width ||
        frame->size.Height != input_size_.Height;
    return *resized || CurrentMonitor() != input_monitor_;
}

HRESULT BorkScreenVideoCapture::TakeReadyFrame(
    BorkNativeFrame *frame, bool *found) {
    *found = false;
    HRESULT result = TakeLatestFrame(frame);
    if (result == S_FALSE) return S_OK;
    if (FAILED(result)) return result;

    bool resized = false;
    if (VideoInputChanged(frame, &resized)) {
        // A window can keep the same size while moving between SDR and HDR
        // displays. Refresh the input type so XVP uses the new white level.
        BorkSizeInt32 size = frame->size;
        ReleaseFrame(frame);
        return RefreshVideoInput(size, resized);
    }
    if (!ShouldEncode(frame->time_hns)) {
        ReleaseFrame(frame);
        return S_OK;
    }
    *found = true;
    return S_OK;
}

HRESULT BorkScreenVideoCapture::PollFrame(
    BorkNativeFrame *frame, bool *found) {
    *found = false;
    HANDLE events[] = {stop_event_, closed_event_, frame_event_};
    DWORD wait = WaitForMultipleObjects(ARRAYSIZE(events), events, FALSE, 2);
    if (wait == WAIT_TIMEOUT) return S_OK;
    if (wait == WAIT_OBJECT_0) return S_FALSE;
    if (wait == WAIT_OBJECT_0 + 1) return HRESULT_FROM_WIN32(ERROR_HANDLE_EOF);
    if (wait == WAIT_FAILED) return bork_hresult_from_last_error();
    return TakeReadyFrame(frame, found);
}

HRESULT BorkScreenVideoCapture::MakeInputSample(
    BorkNativeFrame *frame, IMFSample **sample) {
    IMFMediaBuffer *buffer = nullptr;
    HRESULT result = MFCreateDXGISurfaceBuffer(
        IID_ID3D11Texture2D, frame->texture, 0, FALSE, &buffer);
    IMFSample *value = nullptr;
    if (SUCCEEDED(result)) result = MFCreateSample(&value);
    if (SUCCEEDED(result)) result = value->AddBuffer(buffer);
    LONGLONG time = frame->time_hns - first_time_hns_;
    if (SUCCEEDED(result)) result = value->SetSampleTime(time);
    if (SUCCEEDED(result)) result = value->SetSampleDuration(bork_frame_duration_hns);
    bork_release(buffer);
    if (FAILED(result)) {
        bork_release(value);
        return result;
    }
    *sample = value;
    return S_OK;
}

HRESULT BorkScreenVideoCapture::ProcessVideoProcessor(
    IMFSample *input, IMFSample **output) {
    HRESULT result = video_processor_->ProcessInput(0, input, 0);
    MFT_OUTPUT_DATA_BUFFER data{};
    data.dwStreamID = 0;
    DWORD status = 0;
    if (SUCCEEDED(result)) {
        result = video_processor_->ProcessOutput(0, 1, &data, &status);
    }
    bork_release(data.pEvents);
    if (FAILED(result)) {
        bork_release(data.pSample);
        return result;
    }
    if (data.pSample == nullptr) return E_UNEXPECTED;
    LONGLONG time = 0;
    LONGLONG duration = 0;
    result = input->GetSampleTime(&time);
    if (SUCCEEDED(result)) result = input->GetSampleDuration(&duration);
    if (SUCCEEDED(result)) result = data.pSample->SetSampleTime(time);
    if (SUCCEEDED(result)) result = data.pSample->SetSampleDuration(duration);
    if (FAILED(result)) {
        bork_release(data.pSample);
        return result;
    }
    *output = data.pSample;
    return S_OK;
}

HRESULT BorkScreenVideoCapture::ReadEncoderEvent(BorkEncoderEvent *value) {
    IMFMediaEvent *event = nullptr;
    HRESULT result = encoder_events_->GetEvent(MF_EVENT_FLAG_NO_WAIT, &event);
    if (result == MF_E_NO_EVENTS_AVAILABLE) return S_FALSE;
    if (FAILED(result)) return result;

    value->type = MEError;
    value->status = S_OK;
    result = event->GetStatus(&value->status);
    if (SUCCEEDED(result)) result = event->GetType(&value->type);
    bork_release(event);
    return result;
}

HRESULT BorkScreenVideoCapture::RecordEncoderEvent(
    const BorkEncoderEvent &event) {
    if (FAILED(event.status)) return event.status;
    if (event.type == MEError) return E_FAIL;
    if (event.type == METransformNeedInput) ++encoder_need_input_;
    if (event.type == METransformHaveOutput) ++encoder_have_output_;
    return S_OK;
}

HRESULT BorkScreenVideoCapture::PumpEncoderEvents() {
    BorkEncoderEvent event{};
    HRESULT result = ReadEncoderEvent(&event);
    while (result == S_OK) {
        result = RecordEncoderEvent(event);
        if (FAILED(result)) return result;
        result = ReadEncoderEvent(&event);
    }
    return result == S_FALSE ? S_OK : result;
}

HRESULT BorkScreenVideoCapture::WaitForProgress() {
    HANDLE events[] = {stop_event_, closed_event_};
    DWORD wait = WaitForMultipleObjects(ARRAYSIZE(events), events, FALSE, 2);
    if (wait == WAIT_TIMEOUT) return S_OK;
    if (wait == WAIT_OBJECT_0) return S_FALSE;
    if (wait == WAIT_OBJECT_0 + 1) return HRESULT_FROM_WIN32(ERROR_HANDLE_EOF);
    return wait == WAIT_FAILED ? bork_hresult_from_last_error() : E_UNEXPECTED;
}

HRESULT BorkScreenVideoCapture::MakeEncoderOutputBuffer(
    MFT_OUTPUT_DATA_BUFFER *output) {
    *output = {};
    output->dwStreamID = 0;
    if (encoder_output_sample_ == nullptr) return S_OK;

    encoder_output_sample_->DeleteAllItems();
    IMFMediaBuffer *buffer = nullptr;
    HRESULT result = encoder_output_sample_->GetBufferByIndex(0, &buffer);
    if (SUCCEEDED(result)) result = buffer->SetCurrentLength(0);
    bork_release(buffer);
    if (FAILED(result)) return result;
    encoder_output_sample_->AddRef();
    output->pSample = encoder_output_sample_;
    return S_OK;
}

HRESULT BorkScreenVideoCapture::RequestEncoderKeyFrame(IMFSample *input) {
    LONGLONG time = 0;
    HRESULT result = input->GetSampleTime(&time);
    if (FAILED(result)) return result;
    bool requested = force_key_frame_.exchange(false, std::memory_order_acq_rel);
    if (!requested && time < next_key_frame_hns_) return S_OK;

    // GOP size is only a driver hint. Request a key frame ourselves at least
    // every two seconds so a viewer can recover after losing video data.
    VARIANT force;
    VariantInit(&force);
    force.vt = VT_UI4;
    force.ulVal = 1;
    result = codec_api_->SetValue(&CODECAPI_AVEncVideoForceKeyFrame, &force);
    if (SUCCEEDED(result)) next_key_frame_hns_ = time + bork_key_frame_interval_hns;
    return result;
}

HRESULT BorkScreenVideoCapture::SubmitEncoderInput(IMFSample *input) {
    HRESULT result = RequestEncoderKeyFrame(input);
    if (SUCCEEDED(result)) result = encoder_->ProcessInput(0, input, 0);
    if (SUCCEEDED(result)) --encoder_need_input_;
    return result;
}

HRESULT BorkScreenVideoCapture::TakeEncoderOutput(
    bork_screen_video_frame *output) {
    MFT_OUTPUT_DATA_BUFFER data{};
    HRESULT result = MakeEncoderOutputBuffer(&data);
    DWORD status = 0;
    if (SUCCEEDED(result)) result = encoder_->ProcessOutput(0, 1, &data, &status);
    bork_release(data.pEvents);
    if (FAILED(result)) {
        bork_release(data.pSample);
        return result;
    }
    if (data.pSample == nullptr) return E_UNEXPECTED;
    --encoder_have_output_;
    result = ReadEncoderOutput(data.pSample, output);
    bork_release(data.pSample);
    return result;
}

static bool bork_annex_b_start(const uint8_t *data, size_t size) {
    if (size >= 3 && data[0] == 0 && data[1] == 0 && data[2] == 1) return true;
    return size >= 4 && data[0] == 0 && data[1] == 0 &&
        data[2] == 0 && data[3] == 1;
}

static bool bork_contains_nal(
    const uint8_t *data, size_t size, uint8_t wanted_type) {
    for (size_t index = 0; index + 3 < size; ++index) {
        if (!bork_annex_b_start(data + index, size - index)) continue;
        size_t header = index + (data[index + 2] == 1 ? 3 : 4);
        if (header < size && (data[header] & 0x1f) == wanted_type) return true;
    }
    return false;
}

HRESULT BorkScreenVideoCapture::RefreshSequenceHeader() {
    IMFMediaType *type = nullptr;
    HRESULT result = encoder_->GetOutputCurrentType(0, &type);
    UINT32 size = 0;
    if (SUCCEEDED(result)) result = type->GetBlobSize(MF_MT_MPEG_SEQUENCE_HEADER, &size);
    std::vector<uint8_t> header(size);
    UINT32 written = 0;
    if (SUCCEEDED(result)) {
        result = type->GetBlob(
            MF_MT_MPEG_SEQUENCE_HEADER, header.data(), size, &written);
    }
    bork_release(type);
    if (FAILED(result)) return result;
    header.resize(written);
    bool valid = bork_annex_b_start(header.data(), header.size()) &&
        bork_contains_nal(header.data(), header.size(), 7) &&
        bork_contains_nal(header.data(), header.size(), 8);
    if (!valid) return MF_E_INVALIDMEDIATYPE;
    sequence_header_ = std::move(header);
    return S_OK;
}

HRESULT BorkScreenVideoCapture::CopyEncodedBytes(
    IMFSample *sample, bool *key_frame) {
    IMFMediaBuffer *buffer = nullptr;
    HRESULT result = sample->ConvertToContiguousBuffer(&buffer);
    BYTE *bytes = nullptr;
    DWORD size = 0;
    if (SUCCEEDED(result)) result = buffer->Lock(&bytes, nullptr, &size);
    bool locked = SUCCEEDED(result);
    std::vector<uint8_t> payload;
    if (locked && size != 0) payload.assign(bytes, bytes + size);
    if (locked) buffer->Unlock();
    bork_release(buffer);
    if (FAILED(result)) return result;
    if (!bork_annex_b_start(payload.data(), payload.size())) return MF_E_INVALIDMEDIATYPE;

    if (bork_contains_nal(payload.data(), payload.size(), 5)) *key_frame = true;
    bool has_parameter_sets =
        bork_contains_nal(payload.data(), payload.size(), 7) &&
        bork_contains_nal(payload.data(), payload.size(), 8);
    if (*key_frame && !has_parameter_sets && sequence_header_.empty()) {
        result = RefreshSequenceHeader();
        if (FAILED(result)) return result;
    }

    encoded_frame_.clear();
    if (*key_frame && !has_parameter_sets) {
        encoded_frame_.insert(
            encoded_frame_.end(), sequence_header_.begin(), sequence_header_.end());
    }
    encoded_frame_.insert(encoded_frame_.end(), payload.begin(), payload.end());
    if (encoded_frame_.empty() || encoded_frame_.size() > max_frame_bytes_) {
        return HRESULT_FROM_WIN32(ERROR_BUFFER_OVERFLOW);
    }
    return S_OK;
}

HRESULT BorkScreenVideoCapture::ReadEncoderOutput(
    IMFSample *sample, bork_screen_video_frame *output) {
    UINT32 clean_point = FALSE;
    sample->GetUINT32(MFSampleExtension_CleanPoint, &clean_point);
    bool key_frame = clean_point != FALSE;
    HRESULT result = CopyEncodedBytes(sample, &key_frame);
    if (FAILED(result)) return result;

    LONGLONG time = 0;
    result = sample->GetSampleTime(&time);
    if (FAILED(result)) return result;
    if (time < 0) return E_UNEXPECTED;
    output->data = encoded_frame_.data();
    output->length = static_cast<UINT32>(encoded_frame_.size());
    output->timestamp_us = static_cast<UINT64>(time / 10);
    output->duration_us = bork_frame_duration_us;
    output->key_frame = key_frame ? 1 : 0;
    return S_OK;
}

HRESULT BorkScreenVideoCapture::SubmitFrame(BorkNativeFrame *frame) {
    IMFSample *capture_sample = nullptr;
    HRESULT result = MakeInputSample(frame, &capture_sample);
    IMFSample *converted_sample = nullptr;
    if (result == S_OK) {
        result = ProcessVideoProcessor(capture_sample, &converted_sample);
    }
    if (result == S_OK) result = SubmitEncoderInput(converted_sample);
    bork_release(converted_sample);
    bork_release(capture_sample);
    return result;
}

HRESULT BorkScreenVideoCapture::PumpOnce(
    bork_screen_video_frame *output, bool *produced) {
    *produced = false;
    // Async encoders can ask for several inputs before producing one output.
    // Treat the events as independent counts instead of pairing them up.
    HRESULT result = PumpEncoderEvents();
    if (FAILED(result)) return result;
    if (encoder_have_output_ != 0) {
        result = TakeEncoderOutput(output);
        if (SUCCEEDED(result)) *produced = true;
        return result;
    }
    if (encoder_need_input_ == 0) return WaitForProgress();

    BorkNativeFrame frame;
    bool found = false;
    result = PollFrame(&frame, &found);
    if (result == S_OK && found) result = SubmitFrame(&frame);
    ReleaseFrame(&frame);
    return result;
}

HRESULT BorkScreenVideoCapture::Read(bork_screen_video_frame *frame) {
    if (frame == nullptr) return E_INVALIDARG;
    *frame = {};
    for (;;) {
        bool produced = false;
        HRESULT result = PumpOnce(frame, &produced);
        if (result != S_OK || produced) return result;
    }
}

HRESULT BorkScreenVideoCapture::ForceKeyFrame() {
    force_key_frame_.store(true, std::memory_order_release);
    return S_OK;
}

HRESULT BorkScreenVideoCapture::Stop() {
    if (stop_event_ == nullptr) return S_OK;
    return SetEvent(stop_event_) ? S_OK : bork_hresult_from_last_error();
}

BorkScreenVideoCapture::~BorkScreenVideoCapture() {
    if (frame_pool_ != nullptr && frame_token_.value != 0) {
        frame_pool_->remove_FrameArrived(frame_token_);
    }
    if (capture_item_ != nullptr && closed_token_.value != 0) {
        capture_item_->remove_Closed(closed_token_);
    }
    bork_close_winrt(capture_session_);
    bork_close_winrt(frame_pool_);
    bork_release(capture_session_);
    bork_release(frame_pool_);
    bork_release(capture_item_);
    bork_release(winrt_device_);

    if (transforms_started_ && encoder_ != nullptr) {
        encoder_->ProcessMessage(MFT_MESSAGE_COMMAND_FLUSH, 0);
        encoder_->ProcessMessage(MFT_MESSAGE_NOTIFY_END_STREAMING, 0);
    }
    if (transforms_started_ && video_processor_ != nullptr) {
        video_processor_->ProcessMessage(MFT_MESSAGE_COMMAND_FLUSH, 0);
        video_processor_->ProcessMessage(MFT_MESSAGE_NOTIFY_END_STREAMING, 0);
    }
    bork_shutdown_transform(encoder_);
    bork_release(encoder_output_sample_);
    bork_release(codec_api_);
    bork_release(encoder_);
    bork_release(encoder_events_);
    bork_release(video_processor_control_);
    bork_release(video_processor_);

    bork_release(device_manager_);
    bork_release(device_);
    if (frame_handler_ != nullptr) frame_handler_->Release();
    else if (frame_event_ != nullptr) CloseHandle(frame_event_);
    if (closed_handler_ != nullptr) closed_handler_->Release();
    else if (closed_event_ != nullptr) CloseHandle(closed_event_);
    if (stop_event_ != nullptr) CloseHandle(stop_event_);
    if (mf_started_) MFShutdown();
    if (ro_initialized_) RoUninitialize();
}

extern "C" bork_screen_video_capture *bork_screen_video_capture_start(
    int32_t source_kind, uintptr_t source_handle, uint32_t max_frame_bytes,
    bork_screen_video_info *info_out, int32_t *result_out) {
    if (result_out == nullptr) return nullptr;
    BorkScreenVideoCapture *capture = new (std::nothrow) BorkScreenVideoCapture();
    if (capture == nullptr) {
        *result_out = static_cast<int32_t>(E_OUTOFMEMORY);
        return nullptr;
    }
    HRESULT result = capture->Initialize(
        source_kind, source_handle, max_frame_bytes, info_out);
    if (FAILED(result)) {
        delete capture;
        capture = nullptr;
    }
    *result_out = static_cast<int32_t>(result);
    return capture;
}

extern "C" int32_t bork_screen_video_capture_read(
    bork_screen_video_capture *capture, bork_screen_video_frame *frame_out) {
    if (capture == nullptr) return static_cast<int32_t>(E_INVALIDARG);
    return static_cast<int32_t>(capture->Read(frame_out));
}

extern "C" int32_t bork_screen_video_capture_force_key_frame(
    bork_screen_video_capture *capture) {
    if (capture == nullptr) return static_cast<int32_t>(E_INVALIDARG);
    return static_cast<int32_t>(capture->ForceKeyFrame());
}

extern "C" int32_t bork_screen_video_capture_stop(
    bork_screen_video_capture *capture) {
    if (capture == nullptr) return static_cast<int32_t>(S_OK);
    return static_cast<int32_t>(capture->Stop());
}

extern "C" void bork_screen_video_capture_destroy(
    bork_screen_video_capture *capture) {
    delete capture;
}
