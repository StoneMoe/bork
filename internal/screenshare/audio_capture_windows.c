// Windows process-loopback capture used by the native screen-sharing package.
#define COBJMACROS
#include <initguid.h>
#include <windows.h>
#include <audioclient.h>
#include <mmdeviceapi.h>
#include <objidlbase.h>
#include <propidl.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#define BORK_SCREEN_AUDIO_SAMPLE_RATE 48000
#define BORK_SCREEN_AUDIO_CHANNELS 1
#define BORK_SCREEN_AUDIO_FRAME_SAMPLES 480

// MinGW does not yet ship audioclientactivationparams.h, so keep the small
// Windows SDK ABI definition here until its headers catch up.
typedef enum bork_process_loopback_mode {
    BORK_PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE = 0,
    BORK_PROCESS_LOOPBACK_MODE_EXCLUDE_TARGET_PROCESS_TREE = 1
} bork_process_loopback_mode;

typedef struct bork_audio_client_process_loopback_params {
    DWORD target_process_id;
    bork_process_loopback_mode process_loopback_mode;
} bork_audio_client_process_loopback_params;

typedef enum bork_audio_client_activation_type {
    BORK_AUDIO_CLIENT_ACTIVATION_TYPE_DEFAULT = 0,
    BORK_AUDIO_CLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK = 1
} bork_audio_client_activation_type;

typedef struct bork_audio_client_activation_params {
    bork_audio_client_activation_type activation_type;
    bork_audio_client_process_loopback_params process_loopback_params;
} bork_audio_client_activation_params;

_Static_assert(sizeof(bork_audio_client_activation_params) == 12,
               "Windows process-loopback activation ABI changed");

typedef struct bork_activation_handler {
    IActivateAudioInterfaceCompletionHandler iface;
    LONG refs;
    HANDLE completed;
    HRESULT result;
    IAudioClient *audio_client;
} bork_activation_handler;

typedef struct bork_screen_audio_capture {
    HANDLE sample_ready;
    HANDLE stop_requested;
    IAudioClient *audio_client;
    IAudioCaptureClient *capture_client;
    float *carry;
    UINT32 carry_capacity;
    UINT32 carry_offset;
    UINT32 carry_count;
    BOOL started;
    BOOL uninitialize_com;
} bork_screen_audio_capture;

static bork_activation_handler *bork_handler_from_interface(
    IActivateAudioInterfaceCompletionHandler *iface) {
    return CONTAINING_RECORD(iface, bork_activation_handler, iface);
}

static HRESULT STDMETHODCALLTYPE bork_activation_query_interface(
    IActivateAudioInterfaceCompletionHandler *iface, REFIID iid, void **object) {
    if (object == NULL) {
        return E_POINTER;
    }
    *object = NULL;
    if (IsEqualIID(iid, &IID_IUnknown) ||
        IsEqualIID(iid, &IID_IActivateAudioInterfaceCompletionHandler) ||
        IsEqualIID(iid, &IID_IAgileObject)) {
        *object = iface;
        IActivateAudioInterfaceCompletionHandler_AddRef(iface);
        return S_OK;
    }
    return E_NOINTERFACE;
}

static ULONG STDMETHODCALLTYPE bork_activation_add_ref(
    IActivateAudioInterfaceCompletionHandler *iface) {
    bork_activation_handler *handler = bork_handler_from_interface(iface);
    return (ULONG)InterlockedIncrement(&handler->refs);
}

static ULONG STDMETHODCALLTYPE bork_activation_release(
    IActivateAudioInterfaceCompletionHandler *iface) {
    bork_activation_handler *handler = bork_handler_from_interface(iface);
    ULONG refs = (ULONG)InterlockedDecrement(&handler->refs);
    if (refs == 0) {
        CloseHandle(handler->completed);
        free(handler);
    }
    return refs;
}

static HRESULT STDMETHODCALLTYPE bork_activation_completed(
    IActivateAudioInterfaceCompletionHandler *iface,
    IActivateAudioInterfaceAsyncOperation *operation) {
    bork_activation_handler *handler = bork_handler_from_interface(iface);
    IUnknown *audio_interface = NULL;
    HRESULT activation_result = E_UNEXPECTED;
    HRESULT result = IActivateAudioInterfaceAsyncOperation_GetActivateResult(
        operation, &activation_result, &audio_interface);
    if (SUCCEEDED(result)) {
        result = activation_result;
    }
    if (SUCCEEDED(result)) {
        handler->audio_client = (IAudioClient *)audio_interface;
    } else if (audio_interface != NULL) {
        IUnknown_Release(audio_interface);
    }
    handler->result = result;
    SetEvent(handler->completed);
    return S_OK;
}

static IActivateAudioInterfaceCompletionHandlerVtbl bork_activation_vtable = {
    bork_activation_query_interface,
    bork_activation_add_ref,
    bork_activation_release,
    bork_activation_completed,
};

static bork_activation_handler *bork_new_activation_handler(void) {
    bork_activation_handler *handler = calloc(1, sizeof(*handler));
    if (handler == NULL) {
        return NULL;
    }
    handler->iface.lpVtbl = &bork_activation_vtable;
    handler->refs = 1;
    handler->result = E_UNEXPECTED;
    handler->completed = CreateEventW(NULL, FALSE, FALSE, NULL);
    if (handler->completed == NULL) {
        free(handler);
        return NULL;
    }
    return handler;
}

static HRESULT bork_wait_for_activation(bork_activation_handler *handler) {
    DWORD wait_result = WaitForSingleObject(handler->completed, INFINITE);
    if (wait_result == WAIT_OBJECT_0) {
        return handler->result;
    }
    if (wait_result == WAIT_FAILED) {
        return HRESULT_FROM_WIN32(GetLastError());
    }
    return E_UNEXPECTED;
}

static HRESULT bork_activate_process_loopback(IAudioClient **audio_client) {
    static const WCHAR process_loopback_device[] = L"VAD\\Process_Loopback";
    bork_audio_client_activation_params activation = {0};
    activation.activation_type = BORK_AUDIO_CLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK;
    activation.process_loopback_params.target_process_id = GetCurrentProcessId();
    activation.process_loopback_params.process_loopback_mode =
        BORK_PROCESS_LOOPBACK_MODE_EXCLUDE_TARGET_PROCESS_TREE;

    PROPVARIANT parameters;
    memset(&parameters, 0, sizeof(parameters));
    parameters.vt = VT_BLOB;
    parameters.blob.cbSize = sizeof(activation);
    parameters.blob.pBlobData = (BYTE *)&activation;

    bork_activation_handler *handler = bork_new_activation_handler();
    if (handler == NULL) {
        return E_OUTOFMEMORY;
    }

    IActivateAudioInterfaceAsyncOperation *operation = NULL;
    HRESULT result = ActivateAudioInterfaceAsync(
        process_loopback_device,
        &IID_IAudioClient,
        &parameters,
        &handler->iface,
        &operation);
    if (SUCCEEDED(result)) {
        result = bork_wait_for_activation(handler);
    }
    if (operation != NULL) {
        IActivateAudioInterfaceAsyncOperation_Release(operation);
    }
    if (SUCCEEDED(result)) {
        *audio_client = handler->audio_client;
        handler->audio_client = NULL;
    }
    IActivateAudioInterfaceCompletionHandler_Release(&handler->iface);
    return result;
}

static void bork_release_capture_audio(bork_screen_audio_capture *capture) {
    if (capture->started) {
        IAudioClient_Stop(capture->audio_client);
    }
    if (capture->capture_client != NULL) {
        IAudioCaptureClient_Release(capture->capture_client);
    }
    if (capture->audio_client != NULL) {
        IAudioClient_Release(capture->audio_client);
    }
}

static void bork_close_capture_events(bork_screen_audio_capture *capture) {
    if (capture->sample_ready != NULL) {
        CloseHandle(capture->sample_ready);
    }
    if (capture->stop_requested != NULL) {
        CloseHandle(capture->stop_requested);
    }
}

static void bork_destroy_screen_audio_capture(bork_screen_audio_capture *capture) {
    if (capture == NULL) {
        return;
    }
    bork_release_capture_audio(capture);
    bork_close_capture_events(capture);
    free(capture->carry);
    if (capture->uninitialize_com) {
        CoUninitialize();
    }
    free(capture);
}

static HRESULT bork_initialize_capture_com(bork_screen_audio_capture *capture) {
    HRESULT result = CoInitializeEx(NULL, COINIT_MULTITHREADED);
    if (SUCCEEDED(result)) {
        capture->uninitialize_com = TRUE;
        return S_OK;
    }
    if (result == RPC_E_CHANGED_MODE) {
        return S_OK;
    }
    return result;
}

static HRESULT bork_create_capture_events(bork_screen_audio_capture *capture) {
    capture->sample_ready = CreateEventW(NULL, FALSE, FALSE, NULL);
    capture->stop_requested = CreateEventW(NULL, TRUE, FALSE, NULL);
    if (capture->sample_ready == NULL || capture->stop_requested == NULL) {
        return HRESULT_FROM_WIN32(GetLastError());
    }
    return S_OK;
}

static HRESULT bork_initialize_loopback_client(bork_screen_audio_capture *capture) {
    HRESULT result = bork_activate_process_loopback(&capture->audio_client);
    if (FAILED(result)) {
        return result;
    }

    WAVEFORMATEX format = {0};
    format.wFormatTag = WAVE_FORMAT_IEEE_FLOAT;
    format.nChannels = BORK_SCREEN_AUDIO_CHANNELS;
    format.nSamplesPerSec = BORK_SCREEN_AUDIO_SAMPLE_RATE;
    format.wBitsPerSample = sizeof(float) * 8;
    format.nBlockAlign = format.nChannels * format.wBitsPerSample / 8;
    format.nAvgBytesPerSec = format.nSamplesPerSec * format.nBlockAlign;

    DWORD flags = AUDCLNT_STREAMFLAGS_LOOPBACK |
                  AUDCLNT_STREAMFLAGS_EVENTCALLBACK |
                  AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM |
                  AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY;
    return IAudioClient_Initialize(
        capture->audio_client,
        AUDCLNT_SHAREMODE_SHARED,
        flags,
        0,
        0,
        &format,
        NULL);
}

static HRESULT bork_allocate_capture_buffer(bork_screen_audio_capture *capture) {
    HRESULT result = IAudioClient_GetBufferSize(
        capture->audio_client, &capture->carry_capacity);
    if (FAILED(result)) {
        return result;
    }
    if (capture->carry_capacity == 0) {
        return E_UNEXPECTED;
    }
    capture->carry = calloc(capture->carry_capacity, sizeof(float));
    if (capture->carry == NULL) {
        return E_OUTOFMEMORY;
    }
    return S_OK;
}

static HRESULT bork_start_capture_client(bork_screen_audio_capture *capture) {
    HRESULT result = IAudioClient_GetService(
        capture->audio_client,
        &IID_IAudioCaptureClient,
        (void **)&capture->capture_client);
    if (FAILED(result)) {
        return result;
    }
    result = IAudioClient_SetEventHandle(capture->audio_client, capture->sample_ready);
    if (FAILED(result)) {
        return result;
    }
    result = IAudioClient_Start(capture->audio_client);
    if (SUCCEEDED(result)) {
        capture->started = TRUE;
    }
    return result;
}

static HRESULT bork_prepare_screen_audio_capture(bork_screen_audio_capture *capture) {
    HRESULT result = bork_initialize_capture_com(capture);
    if (FAILED(result)) {
        return result;
    }
    result = bork_create_capture_events(capture);
    if (FAILED(result)) {
        return result;
    }
    result = bork_initialize_loopback_client(capture);
    if (FAILED(result)) {
        return result;
    }
    result = bork_allocate_capture_buffer(capture);
    if (FAILED(result)) {
        return result;
    }
    return bork_start_capture_client(capture);
}

bork_screen_audio_capture *bork_screen_audio_capture_start(int32_t *result_out) {
    bork_screen_audio_capture *capture = calloc(1, sizeof(*capture));
    if (capture == NULL) {
        *result_out = (int32_t)E_OUTOFMEMORY;
        return NULL;
    }
    HRESULT result = bork_prepare_screen_audio_capture(capture);
    if (FAILED(result)) {
        bork_destroy_screen_audio_capture(capture);
        *result_out = (int32_t)result;
        return NULL;
    }
    *result_out = (int32_t)S_OK;
    return capture;
}

static void bork_copy_carry(
    bork_screen_audio_capture *capture,
    float *output,
    UINT32 *written,
    UINT32 frame_count) {
    UINT32 count = frame_count - *written;
    if (count > capture->carry_count) {
        count = capture->carry_count;
    }
    memcpy(
        output + *written,
        capture->carry + capture->carry_offset,
        count * sizeof(float));
    capture->carry_offset += count;
    capture->carry_count -= count;
    *written += count;
}

static HRESULT bork_read_packet_into_carry(bork_screen_audio_capture *capture) {
    UINT32 packet_frames = 0;
    HRESULT result = IAudioCaptureClient_GetNextPacketSize(
        capture->capture_client, &packet_frames);
    if (FAILED(result)) {
        return result;
    }
    if (packet_frames == 0) {
        return S_FALSE;
    }
    if (packet_frames > capture->carry_capacity) {
        return E_UNEXPECTED;
    }

    BYTE *data = NULL;
    DWORD buffer_flags = 0;
    result = IAudioCaptureClient_GetBuffer(
        capture->capture_client,
        &data,
        &packet_frames,
        &buffer_flags,
        NULL,
        NULL);
    if (FAILED(result)) {
        return result;
    }
    if ((buffer_flags & AUDCLNT_BUFFERFLAGS_SILENT) != 0) {
        memset(capture->carry, 0, packet_frames * sizeof(float));
    } else {
        memcpy(capture->carry, data, packet_frames * sizeof(float));
    }
    capture->carry_offset = 0;
    capture->carry_count = packet_frames;
    return IAudioCaptureClient_ReleaseBuffer(capture->capture_client, packet_frames);
}

static HRESULT bork_wait_for_audio_packet(bork_screen_audio_capture *capture) {
    HANDLE events[] = {capture->stop_requested, capture->sample_ready};
    DWORD wait_result = WaitForMultipleObjects(2, events, FALSE, INFINITE);
    if (wait_result == WAIT_OBJECT_0) {
        return S_FALSE;
    }
    if (wait_result == WAIT_OBJECT_0 + 1) {
        return S_OK;
    }
    return HRESULT_FROM_WIN32(GetLastError());
}

static HRESULT bork_fill_carry(bork_screen_audio_capture *capture) {
    for (;;) {
        if (WaitForSingleObject(capture->stop_requested, 0) == WAIT_OBJECT_0) {
            return S_FALSE;
        }
        HRESULT result = bork_read_packet_into_carry(capture);
        if (result != S_FALSE) {
            return result;
        }
        result = bork_wait_for_audio_packet(capture);
        if (result != S_OK) {
            return result;
        }
    }
}

int32_t bork_screen_audio_capture_read(
    bork_screen_audio_capture *capture, float *output, uint32_t frame_count) {
    if (capture == NULL || output == NULL || frame_count != BORK_SCREEN_AUDIO_FRAME_SAMPLES) {
        return (int32_t)E_INVALIDARG;
    }
    if (WaitForSingleObject(capture->stop_requested, 0) == WAIT_OBJECT_0) {
        return (int32_t)S_FALSE;
    }

    UINT32 written = 0;
    while (written < frame_count) {
        if (capture->carry_count == 0) {
            HRESULT result = bork_fill_carry(capture);
            if (result != S_OK) {
                return (int32_t)result;
            }
        }
        bork_copy_carry(capture, output, &written, frame_count);
    }
    return (int32_t)S_OK;
}

int32_t bork_screen_audio_capture_stop(bork_screen_audio_capture *capture) {
    if (capture == NULL) {
        return (int32_t)S_OK;
    }
    if (!SetEvent(capture->stop_requested)) {
        return (int32_t)HRESULT_FROM_WIN32(GetLastError());
    }
    return (int32_t)S_OK;
}

void bork_screen_audio_capture_destroy(bork_screen_audio_capture *capture) {
    bork_destroy_screen_audio_capture(capture);
}
