#ifndef RFPROTO_NATIVE_UI_H
#define RFPROTO_NATIVE_UI_H
#include <SDL3/SDL.h>
#include <stdint.h>
typedef struct native_ui native_ui;
typedef struct native_text native_text;
typedef struct native_event {
    int kind, down, repeat, scan, mods, key, button, phase;
    int64_t finger, device;
    float x, y, wheel, pressure;
    const char *text;
} native_event;
enum { NUI_QUIT=1, NUI_BLUR, NUI_KEY, NUI_TEXT, NUI_MOVE, NUI_BUTTON, NUI_WHEEL, NUI_TOUCH, NUI_EDIT };
native_ui *nui_open(const void *font, size_t length);
void nui_close(native_ui *ui);
SDL_Renderer *nui_renderer(native_ui *ui);
int nui_poll(native_event *out);
float nui_size(native_ui *ui, int *width, int *height);
void nui_begin(native_ui *ui);
void nui_fill(native_ui *ui, float x, float y, float w, float h, unsigned color);
void nui_outline(native_ui *ui, float x, float y, float w, float h, unsigned color);
void nui_clip(native_ui *ui, float x, float y, float w, float h);
void nui_unclip(native_ui *ui);
native_text *nui_text_new(native_ui *ui, const char *text, int size, float density);
void nui_text_free(native_text *text);
void nui_text_draw(native_ui *ui, native_text *text, float x, float y, unsigned color);
void nui_present(native_ui *ui);
int nui_fullscreen(native_ui *ui, int enabled);
void nui_capture(int enabled);
void nui_text_input(native_ui *ui, int enabled, float x, float y, float w, float h);
#endif
