#pragma once

#include <stdint.h>

enum {
    BORK_SCREEN_VIDEO_SOURCE_MONITOR = 0,
    BORK_SCREEN_VIDEO_SOURCE_WINDOW = 1,
    BORK_SCREEN_VIDEO_CODEC_H264_BASELINE = 0,
    BORK_SCREEN_VIDEO_CODEC_H264_MAIN = 1,
};

typedef struct bork_screen_video_info {
    uint32_t width;
    uint32_t height;
    int32_t codec;
} bork_screen_video_info;

typedef struct bork_screen_video_frame {
    const uint8_t *data;
    uint32_t length;
    uint64_t timestamp_us;
    uint32_t duration_us;
    int32_t key_frame;
} bork_screen_video_frame;

#ifdef __cplusplus
class BorkScreenVideoCapture;
typedef BorkScreenVideoCapture bork_screen_video_capture;
extern "C" {
#else
typedef struct bork_screen_video_capture bork_screen_video_capture;
#endif

bork_screen_video_capture *bork_screen_video_capture_start(
    int32_t source_kind,
    uintptr_t source_handle,
    uint32_t max_frame_bytes,
    bork_screen_video_info *info_out,
    int32_t *result_out);
int32_t bork_screen_video_capture_read(
    bork_screen_video_capture *capture,
    bork_screen_video_frame *frame_out);
int32_t bork_screen_video_capture_force_key_frame(bork_screen_video_capture *capture);
int32_t bork_screen_video_capture_stop(bork_screen_video_capture *capture);
void bork_screen_video_capture_destroy(bork_screen_video_capture *capture);

#ifdef __cplusplus
}
#endif
