#include "native_ui.h"
#include <ft2build.h>
#include FT_FREETYPE_H
#include FT_MULTIPLE_MASTERS_H
#include <stdlib.h>
#include <string.h>
#include <math.h>

struct native_window {
    SDL_Window *window;
    SDL_Renderer *renderer;
    uint32_t id;
    int fullscreen;
    float cursor_x, cursor_y;
    struct native_ui *owner;
    struct native_window *next;
};

struct native_ui {
    FT_Library ft;
    FT_Face face;
    int face_weight;
    int face_pixels;
    void *font;
    native_window *windows;
    SDL_Cursor *cursor_arrow;
    SDL_Cursor *cursor_text;
};

struct native_text {
    SDL_Texture *texture;
    float w, h;
};

struct native_texture {
    SDL_Texture *texture;
    int w, h;
};

static uint32_t next_char(const unsigned char **p) {
    uint32_t c = *(*p)++;
    if (c < 128) return c;
    int n = c >= 240 ? 3 : c >= 224 ? 2 : c >= 192 ? 1 : 0;
    if (!n) return 0xfffd;
    c &= (1u << (6 - n)) - 1;
    for (int i = 0; i < n; i++) {
        if ((**p & 0xc0) != 0x80) return 0xfffd;
        c = (c << 6) | (*(*p)++ & 63);
    }
    return c;
}

static int set_face(native_ui *ui, int size, int weight, float density) {
    if (!ui || !ui->face || size <= 0) return -1;
    if (weight >= 100 && weight <= 900 && weight != ui->face_weight) {
        /* Inter is a variable font: set the weight design axis when present. */
        FT_Fixed axis = (FT_Fixed)weight << 16;
        FT_Set_Var_Design_Coordinates(ui->face, 1, &axis);
        ui->face_weight = weight;
    }
    int pixels = (int)ceilf((float)size * (density > 0 ? density : 1));
    if (pixels < 1) pixels = 1;
    if (pixels != ui->face_pixels) {
        int err = FT_Set_Pixel_Sizes(ui->face, 0, (FT_UInt)pixels);
        if (err) return err;
        ui->face_pixels = pixels;
    }
    return 0;
}

static void draw_color(SDL_Renderer *r, unsigned c, unsigned alpha) {
    SDL_SetRenderDrawColor(r, (c >> 16) & 255, (c >> 8) & 255, c & 255, alpha & 255);
}

static native_window *window_for_id(native_ui *ui, SDL_WindowID id) {
    for (native_window *w = ui->windows; w; w = w->next) {
        if (w->id == id) return w;
    }
    return NULL;
}

native_ui *nui_open(const void *font, size_t length) {
    SDL_SetHint(SDL_HINT_MOUSE_TOUCH_EVENTS, "0");
    SDL_SetHint(SDL_HINT_TOUCH_MOUSE_EVENTS, "0");
    if (!SDL_Init(SDL_INIT_VIDEO | SDL_INIT_AUDIO)) return NULL;
    native_ui *ui = calloc(1, sizeof(*ui));
    if (!ui) { SDL_SetError("Cannot allocate native UI"); SDL_Quit(); return NULL; }
    ui->face_weight = -1;
    ui->face_pixels = -1;
    if (FT_Init_FreeType(&ui->ft)) { SDL_SetError("Cannot initialize FreeType"); goto fail; }
    ui->font = malloc(length);
    if (!ui->font) { SDL_SetError("Cannot allocate embedded font"); goto fail; }
    memcpy(ui->font, font, length);
    if (FT_New_Memory_Face(ui->ft, ui->font, (FT_Long)length, 0, &ui->face)) {
        const char *paths[] = {"/usr/share/fonts/TTF/DejaVuSans.ttf", "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"};
        for (size_t i = 0; i < sizeof(paths) / sizeof(paths[0]); ++i)
            if (!FT_New_Face(ui->ft, paths[i], 0, &ui->face)) break;
        if (!ui->face) { SDL_SetError("Cannot load embedded Inter or system DejaVu Sans font"); goto fail; }
        SDL_Log("Embedded Inter unavailable in FreeType; using DejaVu Sans");
    }
    ui->cursor_arrow = SDL_CreateSystemCursor(SDL_SYSTEM_CURSOR_DEFAULT);
    ui->cursor_text = SDL_CreateSystemCursor(SDL_SYSTEM_CURSOR_TEXT);
    return ui;
fail: {
    char error[512];
    SDL_strlcpy(error, SDL_GetError(), sizeof(error));
    nui_close(ui);
    SDL_SetError("%s", error);
    return NULL;
}}

void nui_close(native_ui *ui) {
    if (!ui) return;
    while (ui->windows) nui_window_free(ui->windows);
    if (ui->cursor_arrow) SDL_DestroyCursor(ui->cursor_arrow);
    if (ui->cursor_text) SDL_DestroyCursor(ui->cursor_text);
    if (ui->face) FT_Done_Face(ui->face);
    if (ui->ft) FT_Done_FreeType(ui->ft);
    free(ui->font);
    free(ui);
    SDL_Quit();
}

native_window *nui_window_new(native_ui *ui, const char *title, int w, int h, int minw, int minh) {
    if (!ui) return NULL;
    if (w <= 0 || h <= 0) {
        int dw = 1280, dh = 800;
        nui_display_usable(ui, &dw, &dh);
        w = dw * 9 / 10;
        h = dh * 9 / 10;
    }
    native_window *n = calloc(1, sizeof(*n));
    if (!n) { SDL_SetError("Cannot allocate window"); return NULL; }
    n->owner = ui;
    n->window = SDL_CreateWindow(title, w, h, SDL_WINDOW_RESIZABLE | SDL_WINDOW_HIGH_PIXEL_DENSITY);
    if (!n->window) { free(n); return NULL; }
    n->id = SDL_GetWindowID(n->window);
    if (minw > 0 && minh > 0) SDL_SetWindowMinimumSize(n->window, minw, minh);
    n->renderer = SDL_CreateRenderer(n->window, NULL);
    if (!n->renderer) {
        SDL_DestroyWindow(n->window);
        free(n);
        return NULL;
    }
    SDL_SetRenderDrawBlendMode(n->renderer, SDL_BLENDMODE_BLEND);
    n->next = ui->windows;
    ui->windows = n;
    return n;
}

void nui_window_free(native_window *win) {
    if (!win) return;
    native_ui *ui = win->owner;
    if (ui) {
        native_window **link = &ui->windows;
        while (*link && *link != win) link = &(*link)->next;
        if (*link == win) *link = win->next;
    }
    if (win->renderer) SDL_DestroyRenderer(win->renderer);
    if (win->window) SDL_DestroyWindow(win->window);
    free(win);
}

int nui_poll(native_ui *ui, native_event *out) {
    SDL_Event e;
    while (SDL_PollEvent(&e)) {
        memset(out, 0, sizeof(*out));
        native_window *w = NULL;
        switch (e.type) {
        case SDL_EVENT_QUIT:
            out->kind = NUI_QUIT;
            return 1;
        case SDL_EVENT_WINDOW_CLOSE_REQUESTED:
            w = window_for_id(ui, e.window.windowID);
            if (!w) continue;
            out->kind = NUI_WINDOW_CLOSE;
            out->window = w;
            return 1;
        case SDL_EVENT_WINDOW_FOCUS_LOST:
        case SDL_EVENT_WINDOW_FOCUS_GAINED:
            w = window_for_id(ui, e.window.windowID);
            if (!w) continue;
            out->kind = e.type == SDL_EVENT_WINDOW_FOCUS_LOST ? NUI_BLUR : NUI_FOCUS;
            out->window = w;
            return 1;
        case SDL_EVENT_WINDOW_RESIZED:
        case SDL_EVENT_WINDOW_PIXEL_SIZE_CHANGED:
        case SDL_EVENT_WINDOW_DISPLAY_SCALE_CHANGED:
            w = window_for_id(ui, e.window.windowID);
            if (!w) continue;
            out->kind = NUI_RESIZED;
            out->window = w;
            return 1;
        case SDL_EVENT_KEY_DOWN:
        case SDL_EVENT_KEY_UP:
            w = window_for_id(ui, e.key.windowID);
            if (!w) continue;
            out->kind = NUI_KEY;
            out->window = w;
            out->down = e.key.down;
            out->repeat = e.key.repeat;
            out->scan = e.key.scancode;
            out->mods = e.key.mod;
            out->key = e.key.key;
            return 1;
        case SDL_EVENT_TEXT_INPUT:
            w = window_for_id(ui, e.text.windowID);
            if (!w) continue;
            out->kind = NUI_TEXT;
            out->window = w;
            out->text = e.text.text;
            return 1;
        case SDL_EVENT_TEXT_EDITING:
            w = window_for_id(ui, e.edit.windowID);
            if (!w) continue;
            out->kind = NUI_EDIT;
            out->window = w;
            out->text = e.edit.text;
            return 1;
        case SDL_EVENT_MOUSE_MOTION:
            if (e.motion.which == SDL_TOUCH_MOUSEID) continue;
            w = window_for_id(ui, e.motion.windowID);
            if (!w) continue;
            out->kind = NUI_MOVE;
            out->window = w;
            out->x = e.motion.x;
            out->y = e.motion.y;
            return 1;
        case SDL_EVENT_MOUSE_BUTTON_DOWN:
        case SDL_EVENT_MOUSE_BUTTON_UP:
            if (e.button.which == SDL_TOUCH_MOUSEID) continue;
            w = window_for_id(ui, e.button.windowID);
            if (!w) continue;
            out->kind = NUI_BUTTON;
            out->window = w;
            out->x = e.button.x;
            out->y = e.button.y;
            out->button = e.button.button;
            out->down = e.button.down;
            return 1;
        case SDL_EVENT_MOUSE_WHEEL:
            w = window_for_id(ui, e.wheel.windowID);
            if (!w) continue;
            out->kind = NUI_WHEEL;
            out->window = w;
            out->x = e.wheel.mouse_x;
            out->y = e.wheel.mouse_y;
            out->wheel = e.wheel.y * (e.wheel.direction == SDL_MOUSEWHEEL_FLIPPED ? -1 : 1);
            return 1;
        case SDL_EVENT_FINGER_DOWN:
        case SDL_EVENT_FINGER_MOTION:
        case SDL_EVENT_FINGER_UP:
        case SDL_EVENT_FINGER_CANCELED:
            w = window_for_id(ui, e.tfinger.windowID);
            if (!w) continue;
            out->kind = NUI_TOUCH;
            out->window = w;
            out->finger = e.tfinger.fingerID;
            out->device = e.tfinger.touchID;
            out->x = e.tfinger.x;
            out->y = e.tfinger.y;
            out->pressure = e.tfinger.pressure;
            out->phase = e.type == SDL_EVENT_FINGER_DOWN ? 1 : e.type == SDL_EVENT_FINGER_MOTION ? 2 : e.type == SDL_EVENT_FINGER_UP ? 0 : 3;
            return 1;
        default:
            continue;
        }
    }
    return 0;
}

SDL_Renderer *nui_window_renderer(native_window *win) { return win ? win->renderer : NULL; }

int nui_window_get_size(native_window *win, int *w, int *h, float *density) {
    if (!win) return 0;
    int pw = 0, ph = 0;
    SDL_GetWindowSize(win->window, w, h);
    SDL_GetRenderOutputSize(win->renderer, &pw, &ph);
    float d = *w > 0 ? (float)pw / (float)*w : 1;
    if (d <= 0) d = 1;
    if (density) *density = d;
    return 1;
}

int nui_window_get_position(native_window *win, int *x, int *y) {
    if (!win) return 0;
    SDL_GetWindowPosition(win->window, x, y);
    return 1;
}

void nui_window_set_position(native_window *win, int x, int y) {
    if (win) SDL_SetWindowPosition(win->window, x, y);
}

void nui_window_set_size(native_window *win, int w, int h) {
    if (win && w > 0 && h > 0) SDL_SetWindowSize(win->window, w, h);
}

void nui_window_set_title(native_window *win, const char *title) {
    if (win && title) SDL_SetWindowTitle(win->window, title);
}

void nui_window_raise(native_window *win) {
    if (!win) return;
    SDL_ShowWindow(win->window);
    SDL_RaiseWindow(win->window);
}

void nui_window_hide(native_window *win) {
    if (win) SDL_HideWindow(win->window);
}

int nui_window_focused(native_window *win) {
    return win && (SDL_GetWindowFlags(win->window) & SDL_WINDOW_INPUT_FOCUS) != 0;
}

int nui_window_fullscreen(native_window *win, int enabled) {
    if (!win) return 0;
    int ok = SDL_SetWindowFullscreen(win->window, enabled ? SDL_WINDOW_FULLSCREEN : 0);
    if (ok) win->fullscreen = enabled != 0;
    return ok;
}

int nui_window_is_fullscreen(native_window *win) {
    return win && (SDL_GetWindowFlags(win->window) & SDL_WINDOW_FULLSCREEN) != 0;
}

void nui_window_icon(native_window *win, const void *pixels, int w, int h) {
    if (!win || !pixels || w <= 0 || h <= 0) return;
    SDL_Surface *surface = SDL_CreateSurfaceFrom(w, h, SDL_PIXELFORMAT_RGBA32, (void *)pixels, w * 4);
    if (!surface) return;
    SDL_SetWindowIcon(win->window, surface);
    SDL_DestroySurface(surface);
}

void nui_display_usable(native_ui *ui, int *w, int *h) {
    (void)ui;
    SDL_Rect r = {0, 0, 1280, 800};
    SDL_GetDisplayUsableBounds(SDL_GetPrimaryDisplay(), &r);
    if (w) *w = r.w;
    if (h) *h = r.h;
}

static int begin(native_window *win, float *density) {
    if (!win) return 0;
    int w = 0, h = 0, pw = 0, ph = 0;
    SDL_GetWindowSize(win->window, &w, &h);
    SDL_GetRenderOutputSize(win->renderer, &pw, &ph);
    float d = w > 0 ? (float)pw / (float)w : 1;
    if (d <= 0) d = 1;
    if (density) *density = d;
    SDL_SetRenderScale(win->renderer, d, d);
    SDL_SetRenderDrawBlendMode(win->renderer, SDL_BLENDMODE_BLEND);
    return 1;
}

void nui_begin(native_window *win) {
    if (!begin(win, NULL)) return;
    draw_color(win->renderer, 0x000000, 255);
    SDL_RenderClear(win->renderer);
}

void nui_present(native_window *win) {
    if (win) SDL_RenderPresent(win->renderer);
}

void nui_fill(native_window *win, float x, float y, float w, float h, unsigned c) {
    if (!win || w <= 0 || h <= 0) return;
    SDL_FRect r = {x, y, w, h};
    draw_color(win->renderer, c, 255);
    SDL_RenderFillRect(win->renderer, &r);
}

void nui_fill_a(native_window *win, float x, float y, float w, float h, unsigned c, unsigned alpha) {
    if (!win || w <= 0 || h <= 0 || alpha == 0) return;
    SDL_FRect r = {x, y, w, h};
    draw_color(win->renderer, c, alpha);
    SDL_RenderFillRect(win->renderer, &r);
}

static int rounded_geometry(native_window *win, float x, float y, float w, float h,
                            float radius, unsigned color, unsigned alpha,
                            native_texture *tex, int cover) {
    if (!win || w <= 0 || h <= 0) return 0;
    float maxr = (w < h ? w : h) / 2;
    if (radius > maxr) radius = maxr;
    if (radius < 0) radius = 0;
    const int seg = 8;
    int count = radius > 0.5f ? 4 * (seg + 1) : 4;
    SDL_Vertex v[4 * (seg + 1) + 1];
    int indices[3 * (4 * (seg + 1))];
    float cx = x + w / 2, cy = y + h / 2;
    float r = radius;
    float sx = 0, sy = 0, sw = 1, sh = 1;
    if (tex) {
        float ta = (float)tex->w / (float)tex->h;
        float da = w / h;
        sw = 1; sh = 1; sx = 0; sy = 0;
        if (ta > da) { sw = da / ta; sx = (1 - sw) / 2; }
        else if (ta < da) { sh = ta / da; sy = (1 - sh) / 2; }
    }
    static const float arc_cos[9] = {
        1.00000000f, 0.98078528f, 0.92387953f, 0.83146961f,
        0.70710678f, 0.55557023f, 0.38268343f, 0.19509032f, 0.00000000f
    };
    static const float arc_sin[9] = {
        0.00000000f, 0.19509032f, 0.38268343f, 0.55557023f,
        0.70710678f, 0.83146961f, 0.92387953f, 0.98078528f, 1.00000000f
    };
    const float cr = ((color >> 16) & 255) * (1.0f / 255.0f);
    const float cg = ((color >> 8) & 255) * (1.0f / 255.0f);
    const float cb = (color & 255) * (1.0f / 255.0f);
    const float ca = (alpha & 255) * (1.0f / 255.0f);
    const float inv_w = sw / w;
    const float inv_h = sh / h;
    int n = 0;
    #define ADD(px, py) do { \
        float _px = (px), _py = (py); \
        v[n].position.x = _px; v[n].position.y = _py; \
        v[n].color.r = cr; v[n].color.g = cg; v[n].color.b = cb; v[n].color.a = ca; \
        v[n].tex_coord.x = sx + (_px - x) * inv_w; \
        v[n].tex_coord.y = sy + (_py - y) * inv_h; \
        n++; \
    } while (0)
    if (radius > 0.5f) {
        float lx = x + r, rx = x + w - r;
        float ty = y + r, by = y + h - r;
        for (int i = 0; i <= seg; i++) ADD(lx - arc_cos[i] * r, ty - arc_sin[i] * r);
        for (int i = 0; i <= seg; i++) ADD(rx + arc_sin[i] * r, ty - arc_cos[i] * r);
        for (int i = 0; i <= seg; i++) ADD(rx + arc_cos[i] * r, by + arc_sin[i] * r);
        for (int i = 0; i <= seg; i++) ADD(lx - arc_sin[i] * r, by + arc_cos[i] * r);
    } else {
        ADD(x, y); ADD(x + w, y); ADD(x + w, y + h); ADD(x, y + h);
    }
    #undef ADD
    int ic = 0;
    if (radius > 0.5f) {
        v[n].position.x = cx;
        v[n].position.y = cy;
        v[n].color.r = cr;
        v[n].color.g = cg;
        v[n].color.b = cb;
        v[n].color.a = ca;
        v[n].tex_coord.x = sx + 0.5f * sw;
        v[n].tex_coord.y = sy + 0.5f * sh;
        int center = n++;
        for (int i = 0; i < count; i++) {
            int next = (i + 1) % count;
            indices[ic++] = center;
            indices[ic++] = i;
            indices[ic++] = next;
        }
    } else {
        indices[ic++] = 0; indices[ic++] = 1; indices[ic++] = 2;
        indices[ic++] = 0; indices[ic++] = 2; indices[ic++] = 3;
    }
    if (tex) SDL_SetTextureBlendMode(tex->texture, SDL_BLENDMODE_BLEND);
    return SDL_RenderGeometry(win->renderer, tex ? tex->texture : NULL, v, n, indices, ic);
}

void nui_round(native_window *win, float x, float y, float w, float h, float radius, unsigned color) {
    rounded_geometry(win, x, y, w, h, radius, color, 255, NULL, 0);
}

void nui_round_a(native_window *win, float x, float y, float w, float h, float radius, unsigned color, unsigned alpha) {
    rounded_geometry(win, x, y, w, h, radius, color, alpha, NULL, 0);
}

void nui_clip(native_window *win, float x, float y, float w, float h) {
    if (!win) return;
    SDL_Rect r = {(int)floorf(x), (int)floorf(y), (int)ceilf(w), (int)ceilf(h)};
    SDL_SetRenderClipRect(win->renderer, &r);
}

void nui_unclip(native_window *win) {
    if (win) SDL_SetRenderClipRect(win->renderer, NULL);
}

native_text *nui_text_new(native_window *win, const char *text, int size, int weight, float density) {
    if (!win || !text || !win->owner || !win->owner->face) return NULL;
    native_ui *ui = win->owner;
    if (set_face(ui, size, weight, density)) { SDL_SetError("Cannot size UI font"); return NULL; }
    int width = 0;
    const unsigned char *p = (const unsigned char *)text;
    FT_UInt prev = 0;
    while (*p) {
        FT_UInt glyph = FT_Get_Char_Index(ui->face, next_char(&p));
        FT_Vector kern = {0};
        if (prev && glyph) FT_Get_Kerning(ui->face, prev, glyph, FT_KERNING_DEFAULT, &kern);
        width += kern.x >> 6;
        if (!FT_Load_Glyph(ui->face, glyph, FT_LOAD_DEFAULT)) width += ui->face->glyph->advance.x >> 6;
        prev = glyph;
    }
    int height = (ui->face->size->metrics.height + 63) >> 6;
    int baseline = (ui->face->size->metrics.ascender + 63) >> 6;
    if (height < 1) height = 1;
    if (width < 1) width = 1;
    if (width > 16384 || height > 1024) { SDL_SetError("UI text exceeds texture bounds"); return NULL; }
    SDL_Surface *s = SDL_CreateSurface(width + 4, height + 4, SDL_PIXELFORMAT_RGBA32);
    if (!s) return NULL;
    SDL_ClearSurface(s, 0, 0, 0, 0);
    p = (const unsigned char *)text;
    int pen = 2;
    prev = 0;
    while (*p) {
        FT_UInt glyph = FT_Get_Char_Index(ui->face, next_char(&p));
        FT_Vector kern = {0};
        if (prev && glyph) FT_Get_Kerning(ui->face, prev, glyph, FT_KERNING_DEFAULT, &kern);
        pen += kern.x >> 6;
        if (!FT_Load_Glyph(ui->face, glyph, FT_LOAD_RENDER)) {
            FT_GlyphSlot g = ui->face->glyph;
            FT_Bitmap *b = &g->bitmap;
            int abs_pitch = b->pitch >= 0 ? b->pitch : -b->pitch;
            int base_dx = pen + g->bitmap_left;
            int base_dy = 2 + baseline - g->bitmap_top;
            int is_mono = b->pixel_mode == FT_PIXEL_MODE_MONO;
            for (unsigned y = 0; y < b->rows; y++) {
                int dy = base_dy + (int)y;
                if (dy < 0 || dy >= s->h) continue;
                const unsigned char *row = b->buffer + (b->pitch >= 0 ? y : b->rows - 1 - y) * (unsigned)abs_pitch;
                unsigned char *dst_row = (unsigned char *)s->pixels + dy * s->pitch;
                for (unsigned x = 0; x < b->width; x++) {
                    int dx = base_dx + (int)x;
                    if (dx < 0 || dx >= s->w) continue;
                    unsigned char alpha = is_mono ? (unsigned char)(((row[x >> 3] >> (7 - (x & 7))) & 1) * 255) : row[x];
                    unsigned char *d = dst_row + dx * 4;
                    d[0] = d[1] = d[2] = 255;
                    if (alpha > d[3]) d[3] = alpha;
                }
            }
            pen += g->advance.x >> 6;
        }
        prev = glyph;
    }
    native_text *t = calloc(1, sizeof(*t));
    if (!t) { SDL_DestroySurface(s); SDL_SetError("Cannot allocate UI text"); return NULL; }
    t->texture = SDL_CreateTextureFromSurface(win->renderer, s);
    float d = density > 0 ? density : 1;
    t->w = (float)s->w / d;
    t->h = (float)s->h / d;
    SDL_DestroySurface(s);
    if (!t->texture) { free(t); return NULL; }
    SDL_SetTextureBlendMode(t->texture, SDL_BLENDMODE_BLEND);
    return t;
}

void nui_text_free(native_text *t) {
    if (!t) return;
    if (t->texture) SDL_DestroyTexture(t->texture);
    free(t);
}

void nui_text_draw(native_window *win, native_text *t, float x, float y, unsigned c) {
    if (!win || !t) return;
    SDL_FRect r = {x, y, t->w, t->h};
    SDL_SetTextureColorMod(t->texture, (c >> 16) & 255, (c >> 8) & 255, c & 255);
    SDL_SetTextureAlphaMod(t->texture, 255);
    SDL_RenderTexture(win->renderer, t->texture, NULL, &r);
}

void nui_text_draw_a(native_window *win, native_text *t, float x, float y, unsigned c, unsigned alpha) {
    if (!win || !t) return;
    SDL_FRect r = {x, y, t->w, t->h};
    SDL_SetTextureColorMod(t->texture, (c >> 16) & 255, (c >> 8) & 255, c & 255);
    SDL_SetTextureAlphaMod(t->texture, alpha & 255);
    SDL_RenderTexture(win->renderer, t->texture, NULL, &r);
}

void nui_text_metrics(native_text *t, float *w, float *h) {
    if (w) *w = t ? t->w : 0;
    if (h) *h = t ? t->h : 0;
}

void nui_text_measure(native_window *win, const char *text, int size, int weight, float density, float *w, float *h) {
    if (!win || !text || !win->owner || !win->owner->face) { if (w) *w = 0; if (h) *h = 0; return; }
    native_ui *ui = win->owner;
    if (set_face(ui, size, weight, density)) { if (w) *w = 0; if (h) *h = 0; return; }
    int width = 0;
    const unsigned char *p = (const unsigned char *)text;
    FT_UInt prev = 0;
    while (*p) {
        FT_UInt glyph = FT_Get_Char_Index(ui->face, next_char(&p));
        FT_Vector kern = {0};
        if (prev && glyph) FT_Get_Kerning(ui->face, prev, glyph, FT_KERNING_DEFAULT, &kern);
        width += kern.x >> 6;
        if (!FT_Load_Glyph(ui->face, glyph, FT_LOAD_DEFAULT)) width += ui->face->glyph->advance.x >> 6;
        prev = glyph;
    }
    int height = (ui->face->size->metrics.height + 63) >> 6;
    float d = density > 0 ? density : 1;
    if (w) *w = (float)(width + 4) / d;
    if (h) *h = (float)(height + 4) / d;
}

native_texture *nui_texture_rgba(native_window *win, const void *pixels, int w, int h) {
    if (!win || !pixels || w <= 0 || h <= 0) return NULL;
    SDL_Texture *tex = SDL_CreateTexture(win->renderer, SDL_PIXELFORMAT_RGBA32, SDL_TEXTUREACCESS_STATIC, w, h);
    if (!tex) return NULL;
    if (!SDL_UpdateTexture(tex, NULL, pixels, w * 4)) {
        SDL_DestroyTexture(tex);
        return NULL;
    }
    SDL_SetTextureBlendMode(tex, SDL_BLENDMODE_BLEND);
    SDL_SetTextureScaleMode(tex, SDL_SCALEMODE_LINEAR);
    native_texture *t = calloc(1, sizeof(*t));
    if (!t) { SDL_DestroyTexture(tex); return NULL; }
    t->texture = tex;
    t->w = w;
    t->h = h;
    return t;
}

void nui_texture_free(native_texture *t) {
    if (!t) return;
    if (t->texture) SDL_DestroyTexture(t->texture);
    free(t);
}


void nui_texture_nearest(native_texture *t) {
    if (t) SDL_SetTextureScaleMode(t->texture, SDL_SCALEMODE_NEAREST);
}
void nui_texture_draw(native_window *win, native_texture *t, float x, float y, float w, float h, unsigned alpha) {
    if (!win || !t || w <= 0 || h <= 0) return;
    SDL_FRect r = {x, y, w, h};
    SDL_SetTextureAlphaMod(t->texture, alpha & 255);
    SDL_RenderTexture(win->renderer, t->texture, NULL, &r);
}

void nui_texture_cover(native_window *win, native_texture *t, float x, float y, float w, float h, unsigned alpha) {
    if (!win || !t || w <= 0 || h <= 0) return;
    float ta = (float)t->w / (float)t->h;
    float da = w / h;
    SDL_FRect src = {0, 0, (float)t->w, (float)t->h};
    if (ta > da) {
        src.w = (float)t->h * da;
        src.x = ((float)t->w - src.w) / 2;
    } else if (ta < da) {
        src.h = (float)t->w / da;
        src.y = ((float)t->h - src.h) / 2;
    }
    SDL_FRect dst = {x, y, w, h};
    SDL_SetTextureAlphaMod(t->texture, alpha & 255);
    SDL_RenderTexture(win->renderer, t->texture, &src, &dst);
}

void nui_texture_cover_round(native_window *win, native_texture *t, float x, float y, float w, float h, float radius, unsigned alpha) {
    if (!win || !t) return;
    rounded_geometry(win, x, y, w, h, radius, 0xffffff, alpha, t, 1);
}

void nui_mouse_capture(native_ui *ui, int enabled) {
    (void)ui;
    SDL_CaptureMouse(enabled != 0);
}

void nui_text_input(native_window *win, int enabled, float x, float y, float w, float h) {
    if (!win) return;
    if (enabled) {
        SDL_Rect r = {(int)x, (int)y, (int)w, (int)h};
        SDL_SetTextInputArea(win->window, &r, 0);
        SDL_StartTextInput(win->window);
    } else {
        SDL_StopTextInput(win->window);
    }
}

void nui_cursor(native_window *win, int pointer) {
    if (!win || !win->owner) return;
    SDL_Cursor *c = pointer ? win->owner->cursor_text : win->owner->cursor_arrow;
    if (c) SDL_SetCursor(c);
}
