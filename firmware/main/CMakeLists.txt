idf_component_register(SRCS "main.c" "epd.c" "net.c" "config.c" "console.c" "led.c" "logbuf.c" "ota.c" "guard.c"
                       INCLUDE_DIRS "."
                       REQUIRES driver nvs_flash esp_wifi esp_netif esp_event esp_http_client esp_driver_usb_serial_jtag esp_timer hal app_update esp_app_format esp_partition mbedtls)
