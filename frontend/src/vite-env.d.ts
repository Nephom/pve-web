/// <reference types="vite/client" />
declare module '@novnc/novnc' {
  export default class RFB {
    constructor(target: HTMLElement, url: string, options?: { credentials?: { password?: string } })
    scaleViewport: boolean
    disconnect(): void
    addEventListener(type: string, listener: (event: any) => void): void
  }
}
