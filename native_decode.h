#ifndef RFPROTO_NATIVE_DECODE_H
#define RFPROTO_NATIVE_DECODE_H

#include <stddef.h>
#include <stdint.h>

typedef struct native_video native_video;
typedef struct native_audio native_audio;

/* All access is serialized by the owning Go peer. Video draw/free are main-thread only. */
native_video *native_video_new(int codec, char *error, size_t error_size);
int native_video_push(native_video *video, const uint8_t *data, int size, char *error, size_t error_size);
int native_video_draw(native_video *video, void *renderer, float x, float y, float w, float h, char *error, size_t error_size);
void native_video_free(native_video *video);
uint64_t native_video_inspect(native_video *video, int *width, int *height, int *pitch, uint8_t *rgba);
native_audio *native_audio_new(char *error, size_t error_size);
int native_audio_push(native_audio *audio, const uint8_t *data, int size, int muted, char *error, size_t error_size);
int native_audio_clear(native_audio *audio, char *error, size_t error_size);
void native_audio_free(native_audio *audio);
uint64_t native_audio_inspect(native_audio *audio, int *peak, int *queued);

#endif
