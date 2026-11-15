#ifndef RFPROTO_NATIVE_UI_H
#define RFPROTO_NATIVE_UI_H
#include <SDL3/SDL.h>
#include <stddef.h>
#include <stdint.h>

typedef struct native_ui native_ui;
typedef struct native_window native_window;
typedef struct native_text native_text;
typedef struct native_texture native_texture;

/* One event, always carrying the window it belongs to. */
typedef struct native_event {
    int kind, down, repeat, scan, mods, key, button, phase;
    int64_t finger, device;
    float x, y, wheel, pressure;
    const char *text;
    native_window *window;
} native_event;

enum {
    NUI_QUIT = 1,      /* the last window asked to quit                  */
    NUI_WINDOW_CLOSE,  /* one window's close button                      */
    NUI_BLUR,          /* a window lost keyboard focus                   */
    NUI_FOCUS,         /* a window gained keyboard focus                 */
    NUI_KEY,
    NUI_TEXT,
    NUI_MOVE,
    NUI_BUTTON,
    NUI_WHEEL,
    NUI_TOUCH,
    NUI_EDIT,
    NUI_RESIZED
};

native_ui *nui_open(const void *font, size_t length);
void nui_close(native_ui *ui);

native_window *nui_window_new(native_ui *ui, const char *title, int w, int h, int minw, int minh);
void nui_window_free(native_window *win);
void nui_window_raise(native_window *win);
void nui_window_hide(native_window *win);
void nui_window_set_title(native_window *win, const char *title);
void nui_window_set_size(native_window *win, int w, int h);
int nui_window_get_size(native_window *win, int *w, int *h, float *density);
int nui_window_get_position(native_window *win, int *x, int *y);
void nui_window_set_position(native_window *win, int x, int y);
int nui_window_fullscreen(native_window *win, int enabled);
int nui_window_is_fullscreen(native_window *win);
int nui_window_focused(native_window *win);
void nui_window_icon(native_window *win, const void *pixels, int w, int h);
SDL_Renderer *nui_window_renderer(native_window *win);
void nui_display_usable(native_ui *ui, int *w, int *h);

int nui_poll(native_ui *ui, native_event *out);

void nui_begin(native_window *win);
void nui_present(native_window *win);
void nui_fill(native_window *win, float x, float y, float w, float h, unsigned color);
void nui_fill_a(native_window *win, float x, float y, float w, float h, unsigned color, unsigned alpha);
void nui_round(native_window *win, float x, float y, float w, float h, float radius, unsigned color);
void nui_round_a(native_window *win, float x, float y, float w, float h, float radius, unsigned color, unsigned alpha);
void nui_clip(native_window *win, float x, float y, float w, float h);
void nui_unclip(native_window *win);

native_text *nui_text_new(native_window *win, const char *text, int size, int weight, float density);
void nui_text_free(native_text *text);
void nui_text_draw(native_window *win, native_text *text, float x, float y, unsigned color);
void nui_text_draw_a(native_window *win, native_text *text, float x, float y, unsigned color, unsigned alpha);
void nui_text_metrics(native_text *text, float *w, float *h);
void nui_text_measure(native_window *win, const char *text, int size, int weight, float density, float *w, float *h);

native_texture *nui_texture_rgba(native_window *win, const void *pixels, int w, int h);
void nui_texture_free(native_texture *texture);
void nui_texture_nearest(native_texture *texture);
void nui_texture_draw(native_window *win, native_texture *texture, float x, float y, float w, float h, unsigned alpha);
void nui_texture_cover(native_window *win, native_texture *texture, float x, float y, float w, float h, unsigned alpha);
void nui_texture_cover_round(native_window *win, native_texture *texture, float x, float y, float w, float h, float radius, unsigned alpha);

void nui_mouse_capture(native_ui *ui, int enabled);
void nui_text_input(native_window *win, int enabled, float x, float y, float w, float h);
void nui_cursor(native_window *win, int pointer);
#endif
