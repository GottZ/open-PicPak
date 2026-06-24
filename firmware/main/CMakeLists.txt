# screens.h is a build artifact generated from ../screens-src/gen_screens.py (needs python3 +
# Pillow + the bundled fonts). It is NOT committed. Generate it before building -- the build
# wrapper does this, or run from firmware/:  python3 screens-src/gen_screens.py
# (Guard on EARLY_EXPANSION: this file is also evaluated during IDF's requirements scan, where
# the source dir is not main/.)
if(NOT CMAKE_BUILD_EARLY_EXPANSION AND NOT EXISTS "${CMAKE_CURRENT_LIST_DIR}/screens.h")
    message(FATAL_ERROR "main/screens.h is missing -- generate it first: python3 screens-src/gen_screens.py (needs Pillow)")
endif()
# render_script.h is generated from main/render.be (the Berry render script). Build artifact, not committed.
if(NOT CMAKE_BUILD_EARLY_EXPANSION AND NOT EXISTS "${CMAKE_CURRENT_LIST_DIR}/render_script.h")
    message(FATAL_ERROR "main/render_script.h is missing -- generate it first: python3 host/gen_render.py")
endif()
# font16.h is the 16px glyph table baked from a TTF (host/gen_font16.py, needs Pillow). Build artifact, not committed.
if(NOT CMAKE_BUILD_EARLY_EXPANSION AND NOT EXISTS "${CMAKE_CURRENT_LIST_DIR}/font16.h")
    message(FATAL_ERROR "main/font16.h is missing -- generate it first: python3 host/gen_font16.py (needs Pillow)")
endif()

idf_component_register(SRCS "main.c" "epd.c" "net.c" "netberry.c" "config.c" "console.c" "led.c" "logbuf.c" "ota.c" "guard.c" "fb.c" "dev.c" "store.c"
                       INCLUDE_DIRS "."
                       REQUIRES driver nvs_flash esp_wifi esp_netif esp_event esp_http_client esp_driver_usb_serial_jtag esp_timer hal app_update esp_app_format esp_partition mbedtls berry esp_adc littlefs qrcodegen)
