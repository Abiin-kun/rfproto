#include "native_decode.h"

#include <SDL3/SDL.h>
#include <libavcodec/avcodec.h>
#include <libavutil/imgutils.h>
#include <libavutil/mem.h>
#include <libswscale/swscale.h>
#include <opus/opus.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

struct native_video {
    AVCodecContext *codec;
    AVFrame *frame;
    AVFrame *latest;
    AVPacket *packet;
    struct SwsContext *scale;
    int packet_capacity;
    uint8_t *pixels;
    int width, height, pitch;
    int rgba_width, rgba_height;
    uint64_t serial, uploaded, rgba_serial;
    SDL_Texture *texture;
    SDL_Renderer *renderer;
    SDL_PixelFormat texture_format;
    int texture_width, texture_height;
};

struct native_audio {
    OpusDecoder *decoder;
    SDL_AudioStream *stream;
    opus_int16 pcm[5760 * 2]; /* Maximum 120ms Opus packet, 48kHz stereo. */
    int samples;
    uint64_t decoded_samples;
};

static int fail(char *error, size_t size, const char *message) {
    if (error && size > 0) snprintf(error, size, "%s", message);
    return -1;
}

static int av_fail(char *error, size_t size, const char *operation, int code) {
    char detail[AV_ERROR_MAX_STRING_SIZE];
    av_strerror(code, detail, sizeof(detail));
    if (error && size > 0) snprintf(error, size, "%s: %s", operation, detail);
    return -1;
}

static int ensure_rgba(native_video *v, char *error, size_t error_size) {
    if (!v->serial || !v->latest->data[0]) return 0;
    if (v->rgba_serial == v->serial && v->pixels) return 0;
    int w = v->width, h = v->height;
    if (!v->pixels || w != v->rgba_width || h != v->rgba_height) {
        /* swscale's SSSE3 RGBA path writes complete 16-pixel blocks.
           Keep a padded row stride, including on the final output row. */
        uint8_t *planes[4];
        int pitches[4];
        int allocation = av_image_alloc(planes, pitches, w, h, AV_PIX_FMT_RGBA, 64);
        if (allocation < 0) {
            return av_fail(error, error_size, "allocating decoded frame", allocation);
        }
        av_free(v->pixels);
        v->pixels = planes[0];
        v->rgba_width = w;
        v->rgba_height = h;
        v->pitch = pitches[0];
    }
    v->scale = sws_getCachedContext(v->scale, w, h, v->latest->format,
                                   w, h, AV_PIX_FMT_RGBA, SWS_BILINEAR,
                                   NULL, NULL, NULL);
    if (!v->scale) {
        return fail(error, error_size, "creating video color conversion");
    }
    uint8_t *planes[4] = {v->pixels, NULL, NULL, NULL};
    int pitches[4] = {v->pitch, 0, 0, 0};
    int result = sws_scale(v->scale, (const uint8_t *const *)v->latest->data,
                           v->latest->linesize, 0, h, planes, pitches);
    if (result != h) return fail(error, error_size, "converting decoded frame");
    v->rgba_serial = v->serial;
    return 0;
}

static SDL_PixelFormat direct_yuv_format(const AVFrame *f) {
    if (!f) return SDL_PIXELFORMAT_UNKNOWN;
    if ((f->format == AV_PIX_FMT_YUV420P || f->format == AV_PIX_FMT_YUVJ420P) &&
        f->data[0] && f->data[1] && f->data[2] &&
        f->linesize[0] > 0 && f->linesize[1] > 0 && f->linesize[2] > 0) {
        return SDL_PIXELFORMAT_IYUV;
    }
    if (f->format == AV_PIX_FMT_NV12 &&
        f->data[0] && f->data[1] &&
        f->linesize[0] > 0 && f->linesize[1] > 0) {
        return SDL_PIXELFORMAT_NV12;
    }
    return SDL_PIXELFORMAT_UNKNOWN;
}

native_video *native_video_new(int codec, char *error, size_t error_size) {
    enum AVCodecID id;
    switch (codec) {
    case 1: id = AV_CODEC_ID_H264; break;
    case 2: id = AV_CODEC_ID_VP8; break;
    case 3: id = AV_CODEC_ID_VP9; break;
    default: fail(error, error_size, "unsupported video codec"); return NULL;
    }
    const AVCodec *implementation = avcodec_find_decoder(id);
    if (!implementation) {
        fail(error, error_size, "FFmpeg video decoder is unavailable");
        return NULL;
    }
    native_video *v = calloc(1, sizeof(*v));
    if (!v) { fail(error, error_size, "allocating video decoder"); return NULL; }
    v->codec = avcodec_alloc_context3(implementation);
    v->frame = av_frame_alloc();
    v->latest = av_frame_alloc();
    v->packet = av_packet_alloc();
    if (!v->codec || !v->frame || !v->latest || !v->packet) {
        fail(error, error_size, "allocating FFmpeg decoder buffers");
        native_video_free(v);
        return NULL;
    }
    v->codec->thread_count = 1;
    v->codec->flags |= AV_CODEC_FLAG_LOW_DELAY;
    int result = avcodec_open2(v->codec, implementation, NULL);
    if (result < 0) {
        av_fail(error, error_size, "opening video decoder", result);
        native_video_free(v);
        return NULL;
    }
    return v;
}

static int receive_frames(native_video *v, char *error, size_t error_size) {
    for (;;) {
        int result = avcodec_receive_frame(v->codec, v->frame);
        if (result == AVERROR(EAGAIN) || result == AVERROR_EOF) return 0;
        if (result == AVERROR_INVALIDDATA) {
            av_fail(error, error_size, "decoding video", result);
            return 1; /* Packet loss/startup before a keyframe: request a PLI. */
        }
        if (result < 0) return av_fail(error, error_size, "decoding video", result);
        int w = v->frame->width, h = v->frame->height;
        if (w <= 0 || h <= 0 || w > 16384 || h > 16384) {
            av_frame_unref(v->frame);
            return fail(error, error_size, "unsupported decoded frame dimensions");
        }
        av_frame_unref(v->latest);
        av_frame_move_ref(v->latest, v->frame);
        v->width = w;
        v->height = h;
        v->serial++;
    }
}

int native_video_push(native_video *v, const uint8_t *data, int size, char *error, size_t error_size) {
    /* av_new_packet supplies the padding libavcodec requires, without retaining Go memory. */
    int result;
    if (size > v->packet_capacity) {
        av_packet_unref(v->packet);
        result = av_new_packet(v->packet, size);
        if (result < 0) return av_fail(error, error_size, "allocating encoded video", result);
        v->packet_capacity = size;
    } else {
        v->packet->size = v->packet_capacity;
        result = av_packet_make_writable(v->packet);
        if (result < 0) return av_fail(error, error_size, "preparing encoded video", result);
    }
    v->packet->size = size;
    memcpy(v->packet->data, data, size);
    memset(v->packet->data + size, 0, AV_INPUT_BUFFER_PADDING_SIZE);
    result = avcodec_send_packet(v->codec, v->packet);
    if (result == AVERROR(EAGAIN)) {
        result = receive_frames(v, error, error_size);
        if (result != 0) return result;
        result = avcodec_send_packet(v->codec, v->packet);
    }
    if (result == AVERROR_INVALIDDATA) {
        av_fail(error, error_size, "submitting video packet", result);
        return 1;
    }
    if (result < 0) return av_fail(error, error_size, "submitting video packet", result);
    return receive_frames(v, error, error_size);
}

static int upload_video(native_video *v, SDL_Renderer *renderer, char *error, size_t error_size) {
    if (!v->serial) return 1; /* Waiting is not a decoder/SDL failure. */
    if (!renderer) return fail(error, error_size, "invalid video destination");
    SDL_PixelFormat yuv_fmt = direct_yuv_format(v->latest);
    SDL_PixelFormat target_fmt = yuv_fmt != SDL_PIXELFORMAT_UNKNOWN ? yuv_fmt : SDL_PIXELFORMAT_RGBA32;
    if (!v->texture || v->renderer != renderer || v->texture_width != v->width ||
        v->texture_height != v->height || v->texture_format != target_fmt) {
        SDL_DestroyTexture(v->texture);
        v->texture = SDL_CreateTexture(renderer, target_fmt, SDL_TEXTUREACCESS_STREAMING,
                                       v->width, v->height);
        if (!v->texture && yuv_fmt != SDL_PIXELFORMAT_UNKNOWN) {
            target_fmt = SDL_PIXELFORMAT_RGBA32;
            v->texture = SDL_CreateTexture(renderer, target_fmt, SDL_TEXTUREACCESS_STREAMING,
                                           v->width, v->height);
        }
        if (!v->texture) return fail(error, error_size, SDL_GetError());
        v->renderer = renderer;
        v->texture_format = target_fmt;
        v->texture_width = v->width;
        v->texture_height = v->height;
        v->uploaded = 0;
        if (!SDL_SetTextureScaleMode(v->texture, SDL_SCALEMODE_LINEAR))
            return fail(error, error_size, SDL_GetError());
    }
    if (v->uploaded != v->serial) {
        if (v->texture_format == SDL_PIXELFORMAT_IYUV) {
            if (!SDL_UpdateYUVTexture(v->texture, NULL,
                                      v->latest->data[0], v->latest->linesize[0],
                                      v->latest->data[1], v->latest->linesize[1],
                                      v->latest->data[2], v->latest->linesize[2]))
                return fail(error, error_size, SDL_GetError());
        } else if (v->texture_format == SDL_PIXELFORMAT_NV12) {
            if (!SDL_UpdateNVTexture(v->texture, NULL,
                                     v->latest->data[0], v->latest->linesize[0],
                                     v->latest->data[1], v->latest->linesize[1]))
                return fail(error, error_size, SDL_GetError());
        } else {
            if (ensure_rgba(v, error, error_size) < 0) return -1;
            if (!SDL_UpdateTexture(v->texture, NULL, v->pixels, v->pitch))
                return fail(error, error_size, SDL_GetError());
        }
        v->uploaded = v->serial;
    }
    return 0;
}

int native_video_draw(native_video *v, void *renderer, float x, float y, float w, float h,
                      char *error, size_t error_size) {
    if (!renderer || w <= 0 || h <= 0) return fail(error, error_size, "invalid video destination");
    int result = upload_video(v, renderer, error, error_size);
    if (result != 0) return result;
    float scale = w / v->width;
    if (h / v->height < scale) scale = h / v->height;
    SDL_FRect target = {x + (w - v->width * scale) / 2, y + (h - v->height * scale) / 2,
                        v->width * scale, v->height * scale};
    if (!SDL_RenderTexture(renderer, v->texture, NULL, &target))
        return fail(error, error_size, SDL_GetError());
    return 0;
}

int native_video_draw_rotated(native_video *v, void *renderer, float x, float y, float w, float h,
                              int angle, char *error, size_t error_size) {
    if (!renderer || w <= 0 || h <= 0) return fail(error, error_size, "invalid video destination");
    angle %= 360;
    if (angle < 0) angle += 360;
    if (angle == 0) return native_video_draw(v, renderer, x, y, w, h, error, error_size);
    if (angle != 90 && angle != 180 && angle != 270)
        return fail(error, error_size, "video rotation must be a right angle");
    int result = upload_video(v, renderer, error, error_size);
    if (result != 0) return result;
    float output_width = angle == 180 ? v->width : v->height;
    float output_height = angle == 180 ? v->height : v->width;
    float scale = w / output_width;
    if (h / output_height < scale) scale = h / output_height;
    /* SDL rotates the unrotated destination about its centre; the rotated
       image's bounds are the swapped width/height used for the fit above. */
    float draw_width = v->width * scale;
    float draw_height = v->height * scale;
    SDL_FRect target = {x + (w - draw_width) / 2, y + (h - draw_height) / 2, draw_width, draw_height};
    if (!SDL_RenderTextureRotated(renderer, v->texture, NULL, &target, (double)angle, NULL, SDL_FLIP_NONE))
        return fail(error, error_size, SDL_GetError());
    return 0;
}

uint64_t native_video_inspect(native_video *v, int *width, int *height, int *pitch, uint8_t *rgba) {
    if (v->serial && ensure_rgba(v, NULL, 0) < 0) {
        *width = v->width;
        *height = v->height;
        *pitch = 0;
        return 0;
    }
    *width = v->width;
    *height = v->height;
    *pitch = v->pitch;
    if (v->serial && v->pixels) memcpy(rgba, v->pixels, 4);
    return v->serial;
}

void native_video_free(native_video *v) {
    if (!v) return;
    if (v->texture) SDL_DestroyTexture(v->texture);
    sws_freeContext(v->scale);
    av_free(v->pixels);
    av_packet_free(&v->packet);
    av_frame_free(&v->latest);
    av_frame_free(&v->frame);
    avcodec_free_context(&v->codec);
    free(v);
}

native_audio *native_audio_new(char *error, size_t error_size) {
    native_audio *a = calloc(1, sizeof(*a));
    if (!a) { fail(error, error_size, "allocating audio decoder"); return NULL; }
    int result;
    a->decoder = opus_decoder_create(48000, 2, &result);
    if (!a->decoder) {
        fail(error, error_size, opus_strerror(result));
        native_audio_free(a);
        return NULL;
    }
    return a;
}

int native_audio_clear(native_audio *a, char *error, size_t error_size) {
    if (a->stream && !SDL_ClearAudioStream(a->stream)) return fail(error, error_size, SDL_GetError());
    return 0;
}

int native_audio_push(native_audio *a, const uint8_t *data, int size, int muted,
                      char *error, size_t error_size) {
    int samples = opus_decode(a->decoder, data, size, a->pcm, 5760, 0);
    if (samples < 0) return fail(error, error_size, opus_strerror(samples));
    a->samples = samples;
    a->decoded_samples += samples;
    if (muted) return 0; /* SetMuted already cleared playback; preserve Opus decode state. */
    if (!a->stream) {
        SDL_AudioSpec spec = {.format = SDL_AUDIO_S16, .channels = 2, .freq = 48000};
        a->stream = SDL_OpenAudioDeviceStream(SDL_AUDIO_DEVICE_DEFAULT_PLAYBACK, &spec, NULL, NULL);
        if (!a->stream)
            return fail(error, error_size, SDL_GetError());
        if (!SDL_ResumeAudioStreamDevice(a->stream)) {
            SDL_DestroyAudioStream(a->stream);
            a->stream = NULL;
            return fail(error, error_size, SDL_GetError());
        }
    }
    int queued = SDL_GetAudioStreamQueued(a->stream);
    if (queued < 0) return fail(error, error_size, SDL_GetError());
    /* Clear stale playback rather than accumulating latency: at most 200ms + one packet. */
    if (queued > 48000 * 2 * (int)sizeof(opus_int16) / 5 &&
        native_audio_clear(a, error, error_size) < 0) return -1;
    if (!SDL_PutAudioStreamData(a->stream, a->pcm, samples * 2 * (int)sizeof(opus_int16)))
        return fail(error, error_size, SDL_GetError());
    return 0;
}

uint64_t native_audio_inspect(native_audio *a, int *peak, int *queued) {
    *peak = 0;
    for (int i = 0; i < a->samples * 2; i++) {
        int value = a->pcm[i];
        if (value < 0) value = -value;
        if (value > *peak) *peak = value;
    }
    *queued = a->stream ? SDL_GetAudioStreamQueued(a->stream) : 0;
    return a->decoded_samples;
}

void native_audio_free(native_audio *a) {
    if (!a) return;
    if (a->stream) SDL_DestroyAudioStream(a->stream);
    if (a->decoder) opus_decoder_destroy(a->decoder);
    free(a);
}
