import { expect, type Locator } from '@playwright/test'

// A Sheet enters with a translateX slide (`animate-sheet-in`, 200 ms). Boxes
// read through separate round trips during the slide come from different
// translate offsets, so two boxes that never overlap at rest can appear to.
// Await every animation on the dialog and its descendants before measuring.
//
// Style is resolved first so a dialog mounted in this frame has already
// created its slide. A settled sheet has no animations and returns at once.
// An infinite animation could never settle, so it fails with its names; a
// cancelled animation rejects its `finished` promise and fails the wait. The
// wait is bounded only by the test's own deadline: no sleep, no retry.
export async function waitForSheetSettled(sheet: Locator): Promise<void> {
  await expect(sheet).toBeVisible()
  await sheet.evaluate(async (element) => {
    getComputedStyle(element).transform
    const animations = element.getAnimations({ subtree: true })
    const describe = (animation: Animation) => {
      const name = animation instanceof CSSAnimation ? animation.animationName : animation.id || 'script animation'
      const target = animation.effect instanceof KeyframeEffect ? animation.effect.target : null
      return target instanceof Element ? `${name} on <${target.localName} class="${target.className}">` : name
    }
    const infinite = animations.filter((animation) => animation.effect?.getComputedTiming().endTime === Infinity)
    if (infinite.length > 0)
      throw new Error(`the sheet cannot settle while infinite animations run: ${infinite.map(describe).join('; ')}`)
    await Promise.all(animations.map((animation) => animation.finished))
  })
}
