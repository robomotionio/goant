function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(parseInt('101',2),5);check(parseInt('777',8),511);check(parseInt('z',36),35);check(parseInt('10',10),10);console.log('PASS');
