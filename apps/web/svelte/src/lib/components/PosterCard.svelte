<script lang="ts">
  import { Play, Star, CheckCircle2 } from "lucide-svelte";
  import type { Media } from "$lib/mock-data";
  import { artworkSrc, artworkSrcset } from "$lib/api/artwork-url";

  let { media, variant = "poster" } = $props<{
    media: Media;
    variant?: "poster" | "wide";
  }>();

  const isWide = $derived(variant === "wide");
  const gradient = $derived(`linear-gradient(135deg, ${media.palette[0]}, ${media.palette[1]})`);
  // Route artwork through the resize proxy when we have a (kind, id) the
  // backend can look up. Width matches the CSS card width across density tiers
  // (~180px poster, ~360px wide-card); browsers pick 2x via srcset on retina.
  const artWidth = $derived(isWide ? 360 : 180);
  const rawArt = $derived(isWide ? (media.backdrop ?? media.poster) : (media.poster ?? media.backdrop));
  const artType = $derived<'poster' | 'backdrop'>(isWide ? 'backdrop' : 'poster');
  const art = $derived(artworkSrc(media, artType, artWidth, rawArt));
  const artSrcset = $derived(artworkSrcset(media, artType, artWidth));

  // Continue-Watching items come keyed by media_source_id but the detail page
  // expects the parent movie/series id. Use parentId/parentKind when present.
  const href = $derived.by(() => {
    if (media.parentId && media.parentKind) {
      return media.parentKind === 'series' ? `/tv/${media.parentId}` : `/movies/${media.parentId}`;
    }
    return media.type === 'Series' ? `/tv/${media.id}` : `/movies/${media.id}`;
  });
</script>

<a
  {href}
  class={`group relative shrink-0 ${
    isWide ? "w-[260px] md:w-[320px] lg:w-[360px]" : "w-[140px] md:w-[170px] lg:w-[180px]"
  }`}
>
  <div
    class={`relative overflow-hidden rounded-2xl shadow-poster transition-all duration-300 group-hover:-translate-y-2 group-hover:scale-[1.04] group-hover:ring-[3px] group-hover:ring-white/85 group-hover:shadow-[0_28px_60px_-12px_oklch(0_0_0/0.85)] ${
      isWide ? "aspect-video" : "aspect-[2/3]"
    }`}
    style:background={gradient}
  >
    {#if art}
      <img
        src={art}
        srcset={artSrcset}
        alt={media.title}
        loading="lazy"
        decoding="async"
        class="absolute inset-0 h-full w-full object-cover"
        onerror={(e) => ((e.currentTarget as HTMLElement).style.display = 'none')}
      />
    {/if}

    {#if !art}
      <div
        class="absolute inset-0 opacity-30 mix-blend-overlay"
        style="background-image: radial-gradient(circle at 30% 20%, rgba(255,255,255,0.4), transparent 50%), radial-gradient(circle at 80% 80%, rgba(0,0,0,0.5), transparent 60%);"
      ></div>
    {/if}

    <div
      class="absolute -inset-1 opacity-0 transition-opacity duration-500 group-hover:opacity-100"
      style="background: linear-gradient(115deg, transparent 30%, rgba(255,255,255,0.18) 50%, transparent 70%);"
    ></div>

    {#if art}
      <div class="absolute inset-x-0 bottom-0 h-2/3 bg-gradient-to-t from-black/85 via-black/30 to-transparent"></div>
    {/if}

    <div class="absolute inset-0 flex flex-col justify-between p-4">
      <div class="flex items-center gap-1.5 text-[10px] font-medium uppercase tracking-widest text-white/80">
        {#if media.genres && media.genres.length > 0}
          <span
            class="h-1.5 w-1.5 rounded-full"
            style={`background: ${media.accent}; box-shadow: 0 0 10px ${media.accent};`}
          ></span>
          {media.genres[0]}
        {/if}
      </div>
      <div>
        {#if isWide && media.logo}
          <!-- Continue-Watching style: clearlogo as the title treatment on
               the backdrop, mirroring Netflix's resume cards. -->
          <img
            src={media.logo}
            alt={media.title}
            class="max-h-12 w-auto max-w-[70%] object-contain drop-shadow-xl md:max-h-14"
            style="filter: drop-shadow(0 2px 12px oklch(0 0 0 / 0.6));"
          />
        {:else}
          <h3
            class="font-display text-lg font-bold leading-tight text-white drop-shadow-lg md:text-xl"
            style="text-shadow: 0 2px 12px rgba(0,0,0,0.6);"
          >
            {media.title}
          </h3>
        {/if}
        <div class="mt-1 flex items-center gap-2 text-[11px] text-white/75">
          {#if media.year && media.year > 0}
            <span>{media.year}</span>
          {/if}
          {#if media.year && media.year > 0 && media.rating > 0}
            <span class="opacity-50">•</span>
          {/if}
          {#if media.rating > 0}
            <Star class="h-3 w-3 fill-current text-amber-300" />
            <span>{media.rating.toFixed(1)}</span>
          {/if}
        </div>
      </div>
    </div>

    <div class="absolute inset-0 flex items-center justify-center bg-black/30 opacity-0 transition-opacity duration-300 group-hover:opacity-100">
      <button
        type="button"
        class="flex h-14 w-14 items-center justify-center rounded-full bg-gradient-primary shadow-glow ring-1 ring-white/30 transition-transform hover:scale-110"
        aria-label={`Play ${media.title}`}
      >
        <Play class="h-6 w-6 translate-x-0.5 fill-white text-white" />
      </button>
    </div>

    <!-- Watched badge: shown when fully watched (library flag) OR near-complete (≥90% progress).
         Netflix-style: checkmark in a subtle circle at the top-right of the poster. -->
    {#if media.watched || (typeof media.progress === 'number' && media.progress >= 0.9)}
      <div class="absolute right-2 top-2 z-10 flex h-7 w-7 items-center justify-center rounded-full bg-black/65 ring-1 ring-white/20 backdrop-blur-sm">
        <CheckCircle2 class="h-4 w-4 text-white/90" />
      </div>
    {:else if !isWide && media.type === 'Series' && media.unwatchedCount && media.unwatchedCount > 0}
      <!-- Unplayed episode count badge for TV series — shown when not fully watched -->
      <div class="absolute right-2 top-2 z-10 flex min-w-[1.75rem] items-center justify-center rounded-md bg-primary-glow/90 px-1.5 py-0.5 text-[10px] font-bold text-white shadow-sm ring-1 ring-white/20">
        {media.unwatchedCount > 99 ? '99+' : media.unwatchedCount}
      </div>
    {:else if typeof media.progress === 'number'}
      <!-- In-progress: thin progress bar at the bottom -->
      <div class="absolute inset-x-3 bottom-3">
        <div class="h-[3px] overflow-hidden rounded-full bg-white/20 backdrop-blur">
          <div
            class="h-full rounded-full bg-gradient-primary"
            style={`width: ${Math.max(media.progress * 100, 3)}%`}
          ></div>
        </div>
      </div>
    {/if}
  </div>

  {#if isWide}
    <div class="mt-3 px-1">
      <h4 class="truncate text-sm font-semibold text-foreground">{media.title}</h4>
      <p class="mt-0.5 text-xs text-muted-foreground">
        {#if media.seasonNumber && media.episodeNumber}
          S{media.seasonNumber} E{media.episodeNumber}{media.episodeTitle ? ` • ${media.episodeTitle}` : ''}
        {:else}
          {#if media.year && media.year > 0}{media.year} • {/if}{media.type}
        {/if}
        {#if typeof media.progress === "number"}
          • Resume from {Math.round(media.progress * 100)}%
        {/if}
      </p>
    </div>
  {:else}
    <div class="mt-2 px-1">
      <p class="truncate text-xs text-muted-foreground">
        {#if media.type === "Series" && media.seasons}
          {media.seasons} season{media.seasons !== 1 ? "s" : ""}{media.episodes ? ` • ${media.episodes} ep` : ""}
        {:else if media.type === "Movie" && media.runtime}
          {media.runtime}
        {/if}
      </p>
    </div>
  {/if}
</a>
