import agentserver_sdk


def test_package_importable():
    assert agentserver_sdk.__version__ == "0.1.0"
