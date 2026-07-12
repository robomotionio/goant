function check(a,b){if(a!==b)throw a+" !== "+b;} var r=(function(){function h(){return 7;}return h();})();check(r,7);console.log('PASS');
