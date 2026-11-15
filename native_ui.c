#include "native_ui.h"
#include <ft2build.h>
#include FT_FREETYPE_H
#include <stdlib.h>
#include <string.h>
#include <math.h>

struct native_ui { SDL_Window *window; SDL_Renderer *renderer; FT_Library ft; FT_Face face; void *font; };
struct native_text { SDL_Texture *texture; float w, h; };

native_ui *nui_open(const void *font, size_t length) {
    SDL_SetHint(SDL_HINT_MOUSE_TOUCH_EVENTS, "0");
    SDL_SetHint(SDL_HINT_TOUCH_MOUSE_EVENTS, "0");
    if (!SDL_Init(SDL_INIT_VIDEO | SDL_INIT_AUDIO)) return NULL;
    native_ui *ui = calloc(1, sizeof(*ui));
    if (!ui) { SDL_SetError("Cannot allocate native workspace"); SDL_Quit(); return NULL; }
    ui->window = SDL_CreateWindow("rfproto — Phone workspace", 1280, 800, SDL_WINDOW_RESIZABLE | SDL_WINDOW_HIGH_PIXEL_DENSITY);
    if (!ui->window) goto fail;
    SDL_SetWindowMinimumSize(ui->window, 800, 560);
    ui->renderer = SDL_CreateRenderer(ui->window, NULL);
    if (!ui->renderer) goto fail;
    if (FT_Init_FreeType(&ui->ft)) { SDL_SetError("Cannot initialize FreeType"); goto fail; }
    ui->font = malloc(length);
    if (!ui->font) { SDL_SetError("Cannot allocate embedded font"); goto fail; }
    memcpy(ui->font, font, length);
    if (FT_New_Memory_Face(ui->ft, ui->font, (FT_Long)length, 0, &ui->face)) {
        const char *paths[] = {"/usr/share/fonts/TTF/DejaVuSans.ttf", "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"};
        for (size_t i = 0; i < sizeof(paths)/sizeof(paths[0]); ++i)
            if (!FT_New_Face(ui->ft, paths[i], 0, &ui->face)) break;
        if (!ui->face) { SDL_SetError("Cannot load embedded Inter or system DejaVu Sans font"); goto fail; }
        SDL_Log("Embedded Inter unavailable in FreeType; using DejaVu Sans");
    }
    return ui;
fail: {
    char error[512]; SDL_strlcpy(error, SDL_GetError(), sizeof(error));
    nui_close(ui); SDL_SetError("%s", error); return NULL;
}}
void nui_close(native_ui *ui) {
    if (!ui) return;
    if (ui->face) FT_Done_Face(ui->face);
    if (ui->ft) FT_Done_FreeType(ui->ft);
    free(ui->font);
    SDL_DestroyRenderer(ui->renderer); SDL_DestroyWindow(ui->window); free(ui); SDL_Quit();
}
SDL_Renderer *nui_renderer(native_ui *ui) { return ui->renderer; }
int nui_poll(native_event *out) {
    SDL_Event e;
    while (SDL_PollEvent(&e)) {
        memset(out, 0, sizeof(*out));
        switch (e.type) {
        case SDL_EVENT_QUIT: case SDL_EVENT_WINDOW_CLOSE_REQUESTED: out->kind=NUI_QUIT; break;
        case SDL_EVENT_WINDOW_FOCUS_LOST: out->kind=NUI_BLUR; break;
        case SDL_EVENT_KEY_DOWN: case SDL_EVENT_KEY_UP:
            out->kind=NUI_KEY; out->down=e.key.down; out->repeat=e.key.repeat;
            out->scan=e.key.scancode; out->mods=e.key.mod; out->key=e.key.key; break;
        case SDL_EVENT_TEXT_INPUT: out->kind=NUI_TEXT; out->text=e.text.text; break;
        case SDL_EVENT_TEXT_EDITING: out->kind=NUI_EDIT; out->text=e.edit.text; break;
        case SDL_EVENT_MOUSE_MOTION:
            if (e.motion.which==SDL_TOUCH_MOUSEID) continue;
            out->kind=NUI_MOVE; out->x=e.motion.x; out->y=e.motion.y; break;
        case SDL_EVENT_MOUSE_BUTTON_DOWN: case SDL_EVENT_MOUSE_BUTTON_UP:
            if (e.button.which==SDL_TOUCH_MOUSEID) continue;
            out->kind=NUI_BUTTON; out->x=e.button.x; out->y=e.button.y;
            out->button=e.button.button; out->down=e.button.down; break;
        case SDL_EVENT_MOUSE_WHEEL:
            out->kind=NUI_WHEEL; out->x=e.wheel.mouse_x; out->y=e.wheel.mouse_y;
            out->wheel=e.wheel.y*(e.wheel.direction==SDL_MOUSEWHEEL_FLIPPED ? -1 : 1); break;
        case SDL_EVENT_FINGER_DOWN: case SDL_EVENT_FINGER_MOTION: case SDL_EVENT_FINGER_UP: case SDL_EVENT_FINGER_CANCELED:
            out->kind=NUI_TOUCH; out->finger=e.tfinger.fingerID; out->device=e.tfinger.touchID;
            out->x=e.tfinger.x; out->y=e.tfinger.y; out->pressure=e.tfinger.pressure;
            out->phase=e.type==SDL_EVENT_FINGER_DOWN ? 1 : e.type==SDL_EVENT_FINGER_MOTION ? 2 : e.type==SDL_EVENT_FINGER_UP ? 0 : 3; break;
        default: continue;
        }
        return 1;
    }
    return 0;
}
float nui_size(native_ui *ui, int *w, int *h) {
    int pw, ph; SDL_GetWindowSize(ui->window,w,h); SDL_GetRenderOutputSize(ui->renderer,&pw,&ph);
    float density=*w>0 ? (float)pw / *w : 1;
    if (density<=0) density=1;
    SDL_SetRenderScale(ui->renderer,density,density); return density;
}
static void color(SDL_Renderer *r,unsigned c) { SDL_SetRenderDrawColor(r,(c>>16)&255,(c>>8)&255,c&255,255); }
void nui_begin(native_ui *ui) { color(ui->renderer,0x101010); SDL_RenderClear(ui->renderer); }
void nui_fill(native_ui *ui,float x,float y,float w,float h,unsigned c) { SDL_FRect r={x,y,w,h}; color(ui->renderer,c); SDL_RenderFillRect(ui->renderer,&r); }
void nui_outline(native_ui *ui,float x,float y,float w,float h,unsigned c) { SDL_FRect r={x,y,w,h}; color(ui->renderer,c); SDL_RenderRect(ui->renderer,&r); }
void nui_clip(native_ui *ui,float x,float y,float w,float h) { SDL_Rect r={(int)x,(int)y,(int)w,(int)h}; SDL_SetRenderClipRect(ui->renderer,&r); }
void nui_unclip(native_ui *ui) { SDL_SetRenderClipRect(ui->renderer,NULL); }

static uint32_t next_char(const unsigned char **p) {
    uint32_t c=*(*p)++;
    if (c<128) return c;
    int n=c>=240 ? 3 : c>=224 ? 2 : c>=192 ? 1 : 0;
    if (!n) return 0xfffd;
    c &= (1u<<(6-n))-1;
    for (int i=0;i<n;i++) { if ((**p&0xc0)!=0x80) return 0xfffd; c=(c<<6)|(*(*p)++&63); }
    return c;
}
native_text *nui_text_new(native_ui *ui,const char *text,int size,float density) {
    if (FT_Set_Pixel_Sizes(ui->face,0,(FT_UInt)ceilf(size*density))) { SDL_SetError("Cannot size workspace font"); return NULL; }
    int width=0; const unsigned char *p=(const unsigned char *)text;
    FT_UInt prev=0;
    while (*p) {
        FT_UInt glyph=FT_Get_Char_Index(ui->face,next_char(&p)); FT_Vector kern={0};
        if (prev && glyph) FT_Get_Kerning(ui->face,prev,glyph,FT_KERNING_DEFAULT,&kern);
        width+=kern.x>>6;
        if (!FT_Load_Glyph(ui->face,glyph,FT_LOAD_DEFAULT)) width+=ui->face->glyph->advance.x>>6;
        prev=glyph;
    }
    int height=(ui->face->size->metrics.height+63)>>6;
    int baseline=(ui->face->size->metrics.ascender+63)>>6;
    if (width<1) width=1;
    if (width>32768 || height<1 || height>512) { SDL_SetError("Workspace text exceeds texture bounds"); return NULL; }
    SDL_Surface *s=SDL_CreateSurface(width+4,height+4,SDL_PIXELFORMAT_RGBA32);
    if (!s) return NULL;
    SDL_ClearSurface(s,0,0,0,0);
    p=(const unsigned char *)text; int pen=2; prev=0;
    while (*p) {
        FT_UInt glyph=FT_Get_Char_Index(ui->face,next_char(&p)); FT_Vector kern={0};
        if (prev && glyph) FT_Get_Kerning(ui->face,prev,glyph,FT_KERNING_DEFAULT,&kern);
        pen+=kern.x>>6;
        if (!FT_Load_Glyph(ui->face,glyph,FT_LOAD_RENDER)) {
            FT_GlyphSlot g=ui->face->glyph; FT_Bitmap *b=&g->bitmap;
            for (unsigned y=0;y<b->rows;y++) for (unsigned x=0;x<b->width;x++) {
                int dx=pen+g->bitmap_left+(int)x, dy=2+baseline-g->bitmap_top+(int)y;
                if (dx<0 || dy<0 || dx>=s->w || dy>=s->h) continue;
                const unsigned char *row=b->buffer+(b->pitch>=0 ? y : b->rows-1-y)*abs(b->pitch);
                unsigned char alpha=b->pixel_mode==FT_PIXEL_MODE_MONO ? ((row[x/8]>>(7-x%8))&1)*255 : row[x];
                unsigned char *d=(unsigned char *)s->pixels+dy*s->pitch+dx*4;
                d[0]=d[1]=d[2]=255; if (alpha>d[3]) d[3]=alpha;
            }
            pen+=g->advance.x>>6;
        }
        prev=glyph;
    }
    native_text *t=calloc(1,sizeof(*t));
    if (!t) { SDL_DestroySurface(s); SDL_SetError("Cannot allocate text texture"); return NULL; }
    t->texture=SDL_CreateTextureFromSurface(ui->renderer,s); t->w=s->w/density; t->h=s->h/density;
    SDL_DestroySurface(s);
    if (!t->texture) { free(t); return NULL; }
    SDL_SetTextureBlendMode(t->texture,SDL_BLENDMODE_BLEND); return t;
}
void nui_text_free(native_text *t) { if (t) { SDL_DestroyTexture(t->texture); free(t); } }
void nui_text_draw(native_ui *ui,native_text *t,float x,float y,unsigned c) {
    SDL_FRect r={x,y,t->w,t->h}; SDL_SetTextureColorMod(t->texture,(c>>16)&255,(c>>8)&255,c&255);
    SDL_RenderTexture(ui->renderer,t->texture,NULL,&r);
}
void nui_present(native_ui *ui) { SDL_RenderPresent(ui->renderer); }
int nui_fullscreen(native_ui *ui,int enabled) { return SDL_SetWindowFullscreen(ui->window,enabled!=0); }
void nui_capture(int enabled) { SDL_CaptureMouse(enabled!=0); }
void nui_text_input(native_ui *ui,int enabled,float x,float y,float w,float h) {
    if (enabled) { SDL_Rect r={(int)x,(int)y,(int)w,(int)h}; SDL_SetTextInputArea(ui->window,&r,0); SDL_StartTextInput(ui->window); }
    else SDL_StopTextInput(ui->window);
}
